package xerahttp

import (
	"bytes"
	"context"
	gotls "crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type browserH2Observation struct {
	settings []http2.Setting
	window   uint32
	pseudo   []string
	err      error
}

func observeBrowserH2(peer net.Conn) browserH2Observation {
	defer peer.Close()
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	result := browserH2Observation{}
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(peer, preface); err != nil || string(preface) != http2.ClientPreface {
		result.err = errors.New("invalid HTTP/2 preface")
		return result
	}
	framer := http2.NewFramer(peer, peer)
	framer.ReadMetaHeaders = hpack.NewDecoder(65536, nil)
	for range 2 {
		frame, err := framer.ReadFrame()
		if err != nil {
			result.err = err
			return result
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			frame.ForeachSetting(func(setting http2.Setting) error { result.settings = append(result.settings, setting); return nil })
		case *http2.WindowUpdateFrame:
			result.window = frame.Increment
		default:
			result.err = errors.New("unexpected initial HTTP/2 frame")
			return result
		}
	}
	if err := framer.WriteSettings(); err != nil {
		result.err = err
		return result
	}
	if err := framer.WriteSettingsAck(); err != nil {
		result.err = err
		return result
	}
	for completed := 0; completed < 2; {
		frame, err := framer.ReadFrame()
		if err != nil {
			result.err = err
			return result
		}
		headers, ok := frame.(*http2.MetaHeadersFrame)
		if !ok {
			continue
		}
		if completed == 0 {
			for _, field := range headers.Fields {
				if strings.HasPrefix(field.Name, ":") {
					result.pseudo = append(result.pseudo, field.Name)
				}
				if strings.Contains(field.Name, "order") {
					result.err = errors.New("internal header ordering directive leaked")
					return result
				}
			}
		}
		var block bytes.Buffer
		encoder := hpack.NewEncoder(&block)
		encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		encoder.WriteField(hpack.HeaderField{Name: "content-length", Value: "2"})
		if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: headers.StreamID, EndHeaders: true, BlockFragment: block.Bytes()}); err != nil {
			result.err = err
			return result
		}
		if err := framer.WriteData(headers.StreamID, true, []byte("ok")); err != nil {
			result.err = err
			return result
		}
		completed++
	}
	return result
}

func TestBrowserH2WireProfileAndConnectionReuse(t *testing.T) {
	observed := make(chan browserH2Observation, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			observed <- browserH2Observation{err: err}
			return
		}
		observed <- observeBrowserH2(peer)
	}()
	var dials atomic.Int32
	transport := newBrowserH2Transport(func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}, 0)
	defer transport.CloseIdleConnections()
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		request, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api/", nil)
		request.Header.Set("User-Agent", "Example/1.0")
		response, err := transport.RoundTrip(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		if err != nil || string(data) != "ok" {
			t.Fatal("invalid HTTP/2 response", err)
		}
	}
	result := <-observed
	if result.err != nil {
		t.Fatal(result.err)
	}
	expectedSettings := []http2.Setting{
		{ID: http2.SettingHeaderTableSize, Val: 65536}, {ID: http2.SettingEnablePush, Val: 0},
		{ID: http2.SettingInitialWindowSize, Val: 6291456}, {ID: http2.SettingMaxHeaderListSize, Val: 262144},
	}
	if !reflect.DeepEqual(result.settings, expectedSettings) || result.window != 15663105 || !reflect.DeepEqual(result.pseudo, []string{":method", ":authority", ":scheme", ":path"}) {
		t.Fatalf("incorrect browser HTTP/2 wire profile: %+v", result)
	}
	if dials.Load() != 1 {
		t.Fatal("HTTP/2 connection was not reused")
	}
	request, _ := http.NewRequest("GET", "https://other.example/", nil)
	if _, err := transport.RoundTrip(request); err == nil || dials.Load() != 1 {
		t.Fatal("transport allowed an authority change")
	}
}

func TestBrowserH2BoundsConcurrentRequestsAndWaitCancellation(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 64)
	var closeOnce sync.Once
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/probe/") {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/flow" {
			if r.Method == "POST" {
				if n, err := io.Copy(io.Discard, r.Body); err != nil || n != 20<<20 {
					t.Error("incomplete large HTTP/2 upload", n, err)
				}
				w.Write([]byte("ok"))
				return
			}
			chunk := bytes.Repeat([]byte("flow"), 8192)
			for range 640 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
			return
		}
		started <- struct{}{}
		<-gate
		w.Write([]byte("ok"))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	t.Cleanup(func() { closeOnce.Do(func() { close(gate) }) })
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dialer := &gotls.Dialer{Config: &gotls.Config{RootCAs: roots, NextProtos: []string{"h2"}}}
	var dials atomic.Int32
	transport := newBrowserH2Transport(func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}, 0)
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	completed := make(chan error, 64)
	for range 64 {
		go func() {
			request, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
			response, err := transport.RoundTrip(request)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
			completed <- err
		}()
	}
	for range maxBrowserH2Requests {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("HTTP/2 requests did not reach the server", ctx.Err())
		}
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer waitCancel()
	request, _ := http.NewRequestWithContext(waitCtx, "GET", server.URL, nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("waiting request did not honor cancellation", err)
	}
	select {
	case <-started:
		t.Fatal("request concurrency limit was exceeded")
	default:
	}
	uploadClient := &DefaultDialerClient{client: &http.Client{Transport: transport}, transportConfig: &Config{}}
	if err := uploadClient.PostPacket(ctx, server.URL+"/probe", "session", "0", buf.MergeBytes(nil, []byte("payload"))); err != nil {
		t.Fatal("long downloads starved the upload path", err)
	}
	closeOnce.Do(func() { close(gate) })
	for range 64 {
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() > maxBrowserH2Connections {
		t.Fatal("connection pool exceeded its limit")
	}
	for _, method := range []string{"GET", "POST"} {
		var body io.Reader
		if method == "POST" {
			body = bytes.NewReader(make([]byte, 20<<20))
		}
		request, _ := http.NewRequestWithContext(ctx, method, server.URL+"/flow", body)
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal("large HTTP/2 transfer failed", err)
		}
		n, err := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		expected := int64(20 << 20)
		if method == "POST" {
			expected = 2
		}
		if err != nil || n != expected {
			t.Fatal("HTTP/2 flow control stalled or truncated data", n, err)
		}
	}
}

func TestBrowserH2DialCancellationAndRetirement(t *testing.T) {
	transport := newBrowserH2Transport(func(ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, 0)
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("dial did not honor request cancellation", err)
	}
	transport.CloseIdleConnections()
	request, _ = http.NewRequest("GET", "https://example.com/", nil)
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("retired HTTP/2 transport was reused")
	}
}

func TestBrowserH2ProfileRequiresMatchingTLS(t *testing.T) {
	for _, security := range []string{"tls", "reality"} {
		for _, test := range []struct {
			fingerprint string
			browser     bool
		}{
			{"", true}, {"chrome", true}, {"hellochrome_auto", true}, {"hellochrome_133", true}, {"hellochrome_155", true},
			{"firefox", false}, {"safari", false}, {"hellochrome_120", false}, {"hellochrome_131", false}, {"randomized", false}, {"golang", false},
		} {
			t.Run(security+"/"+test.fingerprint, func(t *testing.T) {
				settings := &internet.MemoryStreamConfig{ProtocolSettings: &Config{}, SecurityType: security}
				if security == "tls" {
					settings.SecuritySettings = &tls.Config{Fingerprint: test.fingerprint}
				} else {
					settings.SecuritySettings = &reality.Config{Fingerprint: test.fingerprint}
				}
				if usesBrowserHTTP2(settings) != test.browser {
					t.Fatal("HTTP/2 profile does not match the selected fingerprint")
				}
				client := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, 443), settings).(*DefaultDialerClient)
				defer client.Close()
				_, browser := client.client.Transport.(*browserH2Transport)
				if browser != test.browser {
					t.Fatalf("unexpected transport: %T", client.client.Transport)
				}
				if !browser {
					if _, ok := client.client.Transport.(*http2.Transport); !ok {
						t.Fatalf("unexpected fallback transport: %T", client.client.Transport)
					}
				}
			})
		}
	}
	if usesBrowserHTTP2(&internet.MemoryStreamConfig{}) {
		t.Fatal("browser HTTP/2 selected without TLS or REALITY")
	}
}

func TestBrowserH2KeepsCertificateVerification(t *testing.T) {
	for _, fingerprint := range []string{"", "chrome", "hellochrome_133", "hellochrome_155"} {
		t.Run(fingerprint, func(t *testing.T) {
			var received atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			address := server.Listener.Addr().(*net.TCPAddr)
			settings := &internet.MemoryStreamConfig{
				ProtocolSettings: &Config{}, SecurityType: "tls",
				SecuritySettings: &tls.Config{ServerName: "example.com", Fingerprint: fingerprint, NextProtocol: []string{"h2"}},
			}
			client := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(address.Port)), settings).(*DefaultDialerClient)
			defer client.Close()
			_, browser := client.client.Transport.(*browserH2Transport)
			if browser != usesBrowserHTTP2(settings) {
				t.Fatal("transport does not match the resolved TLS profile")
			}
			if !browser {
				if _, ok := client.client.Transport.(*http2.Transport); !ok {
					t.Fatalf("unexpected HTTP/2 fallback: %T", client.client.Transport)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api/", nil)
			response, err := client.client.Do(request)
			if response != nil {
				response.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "certificate") || received.Load() != 0 {
				t.Fatal("HTTP/2 profile bypassed certificate verification", err)
			}
		})
	}
}

func BenchmarkBrowserH2RoundTrip(b *testing.B) {
	payload := bytes.Repeat([]byte("data"), 8192)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dialer := &gotls.Dialer{Config: &gotls.Config{RootCAs: roots, NextProtos: []string{"h2"}}}
	dial := func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	for _, mode := range []string{"native", "browser"} {
		b.Run(mode, func(b *testing.B) {
			var transport http.RoundTripper
			if mode == "browser" {
				transport = newBrowserH2Transport(dial, 0)
			} else {
				transport = &http2.Transport{DialTLSContext: func(ctx context.Context, _, _ string, _ *gotls.Config) (net.Conn, error) { return dial(ctx) }}
			}
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				request, _ := http.NewRequest("GET", server.URL, nil)
				response, err := client.Do(request)
				if err != nil {
					b.Fatal(err)
				}
				n, err := io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if err != nil || n != int64(len(payload)) {
					b.Fatal("incomplete HTTP/2 benchmark transfer", n, err)
				}
			}
		})
	}
}
