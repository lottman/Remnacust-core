package splithttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
)

type lifecycleRoundTrip func(*http.Request) (*http.Response, error)

func (f lifecycleRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenStreamCancellationReleasesRequest(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	finished := make(chan struct{})
	c := &DefaultDialerClient{transportConfig: &Config{}, client: &http.Client{Transport: lifecycleRoundTrip(func(r *http.Request) (*http.Response, error) {
		defer close(finished)
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return nil, io.ErrClosedPipe
		}
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		r, _, _, err := c.OpenStream(ctx, "http://localhost/", "session", nil, false)
		if r != nil {
			r.Close()
		}
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenStream ignored cancellation")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("request goroutine survived cancellation")
	}
}

func TestPostPacketCancellationReleasesRequest(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	c := &DefaultDialerClient{transportConfig: &Config{}, client: &http.Client{Transport: lifecycleRoundTrip(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		close(started)
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return nil, io.ErrClosedPipe
		}
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- c.PostPacket(ctx, "http://localhost/", "session", "0", buf.MergeBytes(nil, []byte("payload")))
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PostPacket ignored cancellation")
	}
}

func TestOpenStreamRetainsFailureAfterConnection(t *testing.T) {
	want := errors.New("test stream reset")
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	c := &DefaultDialerClient{transportConfig: &Config{}, client: &http.Client{Transport: lifecycleRoundTrip(func(r *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(r.Context()).GotConn(httptrace.GotConnInfo{Conn: local})
		return nil, want
	})}}
	r, _, _, err := c.OpenStream(context.Background(), "http://localhost/", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, want) {
		t.Fatalf("stream lost the request error: %v", err)
	}
}

func TestHTTP1PacketWaitsForAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	addr := server.Listener.Addr().(*net.TCPAddr)
	c := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(addr.Port)), &internet.MemoryStreamConfig{ProtocolSettings: &Config{}}).(*DefaultDialerClient)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.PostPacket(ctx, server.URL, "session", "0", buf.MergeBytes(nil, []byte("payload")))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("packet acknowledgement lost: %v", err)
	}
}

func TestClientRejectsRedirects(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/redirected") {
			redirected.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	addr := server.Listener.Addr().(*net.TCPAddr)
	c := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(addr.Port)), &internet.MemoryStreamConfig{ProtocolSettings: &Config{}}).(*DefaultDialerClient)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, _, _, err := c.OpenStream(ctx, server.URL, "", nil, false)
	if err == nil {
		_, err = io.ReadAll(r)
		r.Close()
	}
	if redirected.Load() != 0 || err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect followed or status lost: visits=%d err=%v", redirected.Load(), err)
	}
}

func TestConnectionCloseCancelsPendingUpload(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer server.Close()
	defer func() { server.CloseClientConnections() }()
	addr := server.Listener.Addr().(*net.TCPAddr)
	settings := &internet.MemoryStreamConfig{ProtocolSettings: &Config{Mode: "packet-up"}}
	defer settings.Close()
	conn, err := Dial(context.Background(), xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(addr.Port)), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("packet did not arrive")
	}
	conn.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("packet request survived connection close")
	}
}

func TestPacketUploadsBoundPendingAcknowledgements(t *testing.T) {
	var active atomic.Int32
	started := make(chan struct{}, 64)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
		} else {
			io.Copy(io.Discard, r.Body)
			active.Add(1)
			started <- struct{}{}
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	addr := server.Listener.Addr().(*net.TCPAddr)
	settings := &internet.MemoryStreamConfig{ProtocolSettings: &Config{Mode: "packet-up", ScMaxEachPostBytes: &RangeConfig{From: 1, To: 1}}}
	defer settings.Close()
	conn, err := Dial(context.Background(), xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(addr.Port)), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(strings.Repeat("p", 100))); err != nil {
		t.Fatal(err)
	}
	for range 16 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("bounded upload window did not fill")
		}
	}
	select {
	case <-started:
		t.Fatalf("pending uploads exceeded the acknowledgement window: %d", active.Load())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHTTP2CoalescesStalledTLSConnections(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var opened atomic.Int32
	var connections []net.Conn
	var access sync.Mutex
	accepted := make(chan struct{}, 64)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			access.Lock()
			connections = append(connections, conn)
			access.Unlock()
			opened.Add(1)
			accepted <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-stopped
		access.Lock()
		defer access.Unlock()
		for _, conn := range connections {
			conn.Close()
		}
	})
	addr := listener.Addr().(*net.TCPAddr)
	c := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(addr.Port)), &internet.MemoryStreamConfig{
		ProtocolSettings: &Config{}, SecuritySettings: &tls.Config{ServerName: "localhost", NextProtocol: []string{"h2"}, Fingerprint: "golang"},
	}).(*DefaultDialerClient)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 32)
	for range 32 {
		go func() {
			r, _, _, err := c.OpenStream(ctx, "https://localhost/", "session", nil, false)
			if r != nil {
				r.Close()
			}
			results <- err
		}()
	}
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("TLS dial did not start")
	}
	select {
	case <-accepted:
		t.Fatalf("HTTP/2 opened parallel stalled TLS dials: %d", opened.Load())
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	for range 32 {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("stalled TLS request cancellation: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("stalled TLS request outlived cancellation")
		}
	}
}
