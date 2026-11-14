package splithttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"

	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
)

// interface to abstract between use of browser dialer, vs net/http
type DialerClient interface {
	IsClosed() bool

	// ctx, url, sessionId, body, uploadOnly
	OpenStream(context.Context, string, string, io.Reader, bool) (io.ReadCloser, net.Addr, net.Addr, error)

	// ctx, url, sessionId, seqStr, body, contentLength
	PostPacket(context.Context, string, string, string, buf.MultiBuffer) error
}

// implements splithttp.DialerClient in terms of direct network connections
type DefaultDialerClient struct {
	transportConfig *Config
	client          *http.Client
	closed          atomic.Bool
	httpVersion     string
	uploadClient    *http.Client
}

func (c *DefaultDialerClient) IsClosed() bool {
	return c.closed.Load()
}

func (c *DefaultDialerClient) OpenStream(ctx context.Context, url string, sessionId string, body io.Reader, uploadOnly bool) (io.ReadCloser, net.Addr, net.Addr, error) {
	// this is done when the TCP/UDP connection to the server was established,
	// and we can unblock the Dial function and print correct net addresses in
	// logs
	type dialResult struct {
		remote net.Addr
		local  net.Addr
		err    error
	}
	connected := make(chan dialResult, 1)
	var notifyOnce sync.Once
	notify := func(result dialResult) { notifyOnce.Do(func() { connected <- result }) }
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			notify(dialResult{remote: connInfo.Conn.RemoteAddr(), local: connInfo.Conn.LocalAddr()})
		},
	})
	stopCancel := context.AfterFunc(ctx, cancel)
	defer stopCancel()

	method := "GET" // stream-down
	if body != nil {
		method = c.transportConfig.GetNormalizedUplinkHTTPMethod() // stream-up/one
	}
	req, err := http.NewRequestWithContext(requestCtx, method, url, body)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	c.transportConfig.FillStreamRequest(req, sessionId, "")

	w := &WaitReadCloser{wait: make(chan struct{}), cancel: cancel}
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			if !uploadOnly && requestCtx.Err() == nil { // stream-down is enough
				c.closed.Store(true)
			}
			notify(dialResult{err: err})
			common.Close(body)
			w.Fail(err)
			return
		}
		if resp.StatusCode != http.StatusOK {
			err := errors.New("XHTTP stream rejected with HTTP status ", resp.StatusCode)
			notify(dialResult{err: err})
			resp.Body.Close()
			common.Close(body)
			w.Fail(err)
			return
		}
		if uploadOnly { // stream-up
			_, readErr := io.Copy(io.Discard, resp.Body)
			resp.Body.Close() // if it is called immediately, the upload will be interrupted also
			common.Close(body)
			if readErr != nil {
				w.Fail(readErr)
			} else {
				w.Close()
			}
			return
		}
		w.Set(resp.Body)
	}()

	select {
	case result := <-connected:
		if err := ctx.Err(); err != nil {
			w.Close()
			return nil, nil, nil, err
		}
		if result.err != nil {
			w.Close()
			return nil, nil, nil, result.err
		}
		return w, result.remote, result.local, nil
	case <-ctx.Done():
		w.Close()
		return nil, nil, nil, ctx.Err()
	}
}

func (c *DefaultDialerClient) PostPacket(ctx context.Context, url string, sessionId string, seqStr string, payload buf.MultiBuffer) error {
	method := c.transportConfig.GetNormalizedUplinkHTTPMethod()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		buf.ReleaseMulti(payload)
		return err
	}
	if err := c.transportConfig.FillPacketRequest(req, sessionId, seqStr, payload); err != nil {
		return err
	}
	client := c.client
	if c.uploadClient != nil {
		client = c.uploadClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.closed.Store(true)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("bad status code:", resp.Status)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// HTTP/1.1 and HTTP/2 will close itself, we only handle HTTP/3 here
func (c *DefaultDialerClient) Close() error {
	c.closed.Store(true)
	c.client.CloseIdleConnections()
	if c.uploadClient != nil {
		c.uploadClient.CloseIdleConnections()
	}
	transport := c.client.Transport
	if h3Transport, ok := transport.(*http3.Transport); ok {
		return h3Transport.Close()
	}
	return nil
}

type WaitReadCloser struct {
	wait    chan struct{}
	mu      sync.Mutex
	reader  io.ReadCloser
	closed  bool
	ready   bool
	cancel  context.CancelFunc
	readErr error
}

func (w *WaitReadCloser) Set(rc io.ReadCloser) {
	w.mu.Lock()
	if w.closed || w.ready {
		w.mu.Unlock()
		rc.Close()
		return
	}
	w.reader = rc
	w.ready = true
	close(w.wait)
	w.mu.Unlock()
}

func (w *WaitReadCloser) Read(b []byte) (int, error) {
	<-w.wait
	w.mu.Lock()
	rc, closed, readErr := w.reader, w.closed, w.readErr
	w.mu.Unlock()
	if readErr != nil {
		return 0, readErr
	}
	if closed || rc == nil {
		return 0, io.ErrClosedPipe
	}
	return rc.Read(b)
}

func (w *WaitReadCloser) Fail(err error) {
	w.mu.Lock()
	w.readErr = err
	w.mu.Unlock()
	w.Close()
}

func (w *WaitReadCloser) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	rc := w.reader
	w.reader = nil
	if !w.ready {
		w.ready = true
		close(w.wait)
	}
	w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
	if rc != nil {
		return rc.Close()
	}
	return nil
}
