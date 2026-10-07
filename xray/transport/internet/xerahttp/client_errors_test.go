package xerahttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
)

func TestOpenStreamPreservesRequestErrorAfterGotConn(t *testing.T) {
	want := errors.New("test stream reset")
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	c := &DefaultDialerClient{transportConfig: &Config{}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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

type failedUploadResponse struct{ err error }

func (r failedUploadResponse) Read([]byte) (int, error) { return 0, r.err }
func (r failedUploadResponse) Close() error             { return nil }

func TestUploadStreamPreservesResponseReadError(t *testing.T) {
	want := errors.New("upload response interrupted")
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	c := &DefaultDialerClient{transportConfig: &Config{}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(r.Context()).GotConn(httptrace.GotConnInfo{Conn: local})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: failedUploadResponse{want}}, nil
	})}}
	r, _, _, err := c.OpenStream(context.Background(), "http://localhost/", "session", strings.NewReader("data"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, want) {
		t.Fatalf("upload response error lost: %v", err)
	}
}

func TestOpenStreamReportsRejectedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	c := &DefaultDialerClient{transportConfig: &Config{}, client: server.Client()}
	r, _, _, err := c.OpenStream(context.Background(), server.URL, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("stream lost HTTP status: %v", err)
	}
}

func TestXeraClientRejectsRedirects(t *testing.T) {
	for _, packet := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "packet"}[packet], func(t *testing.T) {
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
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			if packet {
				err = c.PostPacket(ctx, server.URL, "session", "0", buf.MergeBytes(nil, []byte("payload")))
			} else {
				var r io.ReadCloser
				r, _, _, err = c.OpenStream(ctx, server.URL, "", nil, false)
				if err == nil {
					_, err = io.ReadAll(r)
					r.Close()
				}
			}
			if redirected.Load() != 0 || err == nil || !strings.Contains(err.Error(), "302") {
				t.Fatalf("redirect followed or status lost: visits=%d err=%v", redirected.Load(), err)
			}
		})
	}
}

func TestDialRejectsMissingDownloadDestination(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("invalid download settings panicked: %v", r)
		}
	}()
	config := &Config{DownloadSettings: &internet.StreamConfig{ProtocolName: protocolName}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := Dial(ctx, xnet.TCPDestination(xnet.LocalHostIP, 1), &internet.MemoryStreamConfig{ProtocolSettings: config})
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "download") {
		t.Fatalf("invalid download settings were not rejected: %v", err)
	}
}
