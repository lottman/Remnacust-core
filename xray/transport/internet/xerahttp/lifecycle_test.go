package xerahttp

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/pipe"
)

type trackedCloser struct {
	closed atomic.Int32
	err    error
}

func (c *trackedCloser) Read(b []byte) (int, error)  { return 0, io.EOF }
func (c *trackedCloser) Write(b []byte) (int, error) { return len(b), nil }
func (c *trackedCloser) Close() error                { c.closed.Add(1); return c.err }

func TestWaitReadCloserConcurrentLifecycle(t *testing.T) {
	for range 200 {
		w := &WaitReadCloser{Wait: make(chan struct{})}
		r := &trackedCloser{}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); w.Set(r) }()
		go func() { defer wg.Done(); w.Read(make([]byte, 1)) }()
		go func() { defer wg.Done(); w.Close() }()
		wg.Wait()
		w.Close()
		if r.closed.Load() != 1 {
			t.Fatalf("body closed %d times", r.closed.Load())
		}
		if _, err := w.Read(make([]byte, 1)); err != io.ErrClosedPipe {
			t.Fatalf("read after close: %v", err)
		}
	}
}

func TestTransportConnClosePreservesReaderError(t *testing.T) {
	want := errors.New("reader close failed")
	r := &trackedCloser{err: want}
	w := &trackedCloser{}
	var callbacks atomic.Int32
	c := &transportConn{reader: r, writer: w, onClose: func() { callbacks.Add(1) }}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(); err != want {
				t.Errorf("close: %v", err)
			}
		}()
	}
	wg.Wait()
	if r.closed.Load() != 1 || w.closed.Load() != 1 || callbacks.Load() != 1 {
		t.Fatal("close was not idempotent")
	}
}

func TestUploadHeapReleasesPoppedPayload(t *testing.T) {
	h := uploadHeap{}
	heap.Push(&h, Packet{Payload: make([]byte, 1<<20)})
	heap.Pop(&h)
	if h[:cap(h)][0].Payload != nil {
		t.Fatal("popped payload is still retained")
	}
}

func TestOpenStreamCloseCancelsPendingResponse(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	c := &DefaultDialerClient{transportConfig: &Config{}, client: server.Client()}
	r, _, _, err := c.OpenStream(context.Background(), server.URL, "session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not arrive")
	}
	r.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("request survived stream close")
	}
}

func TestUploadWriterHonorsSmallLimit(t *testing.T) {
	r, w := pipe.New(pipe.WithSizeLimit(0))
	defer r.Interrupt()
	upload := uploadWriter{Writer: w, maxLen: 3}
	result := make(chan error, 1)
	go func() { _, err := upload.Write([]byte("abcdefgh")); w.Close(); result <- err }()
	var got []byte
	for {
		mb, err := r.ReadMultiBuffer()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if mb.Len() > 3 {
			t.Fatalf("chunk exceeds limit: %d", mb.Len())
		}
		for _, b := range mb {
			got = append(got, b.Bytes()...)
		}
		buf.ReleaseMulti(mb)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdefgh" {
		t.Fatalf("payload: %q", got)
	}
}

func TestHTTP1UploadResponseAndConnectionLifecycle(t *testing.T) {
	var opened atomic.Int32
	closed := make(chan struct{}, 4)
	var status atomic.Int32
	status.Store(http.StatusOK)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(payload, []byte("payload")) {
			t.Errorf("body: %q, %v", payload, err)
		}
		w.WriteHeader(int(status.Load()))
	}))
	server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
		if s == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	addr := server.Listener.Addr().(*net.TCPAddr)
	c := createHTTPClient(xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(addr.Port)), &internet.MemoryStreamConfig{ProtocolSettings: &Config{}}).(*DefaultDialerClient)
	defer c.Close()
	for range 3 {
		if err := c.PostPacket(context.Background(), server.URL, "session", "0", buf.MergeBytes(nil, []byte("payload"))); err != nil {
			t.Fatal(err)
		}
	}
	if opened.Load() != 1 {
		t.Fatalf("expected one reused connection, got %d", opened.Load())
	}
	status.Store(http.StatusServiceUnavailable)
	if err := c.PostPacket(context.Background(), server.URL, "session", "1", buf.MergeBytes(nil, []byte("payload"))); err == nil {
		t.Fatal("server rejection was ignored")
	}
	c.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle upload connection was not closed")
	}
}
