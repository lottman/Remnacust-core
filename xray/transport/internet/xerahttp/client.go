package xerahttp

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

type DialerClient interface {
	IsClosed() bool

	OpenStream(context.Context, string, string, io.Reader, bool) (io.ReadCloser, net.Addr, net.Addr, error)

	PostPacket(context.Context, string, string, string, buf.MultiBuffer) error
}

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
	if p := c.transportConfig.CustomDownlinkPadding; p != nil {
		if err := p.Validate(); err != nil {
			return nil, nil, nil, err
		}
	}
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
		GotConn: func(info httptrace.GotConnInfo) {
			notify(dialResult{remote: info.Conn.RemoteAddr(), local: info.Conn.LocalAddr()})
		},
	})
	stopCancel := context.AfterFunc(ctx, cancel)
	defer stopCancel()
	method := "GET"
	if body != nil {
		method = c.transportConfig.GetNormalizedUplinkHTTPMethod()
		if p := c.transportConfig.CustomDownlinkPadding; p != nil && p.Uplink {
			closer, ok := body.(io.ReadCloser)
			if !ok {
				closer = io.NopCloser(body)
			}
			body = &paddedUplinkReader{ReadCloser: closer, config: p}
		}
	}
	req, err := http.NewRequestWithContext(requestCtx, method, url, body)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	c.transportConfig.FillStreamRequest(req, sessionId, "")
	padding := c.transportConfig.CustomDownlinkPadding
	if padding != nil && (!uploadOnly || padding.Uplink) {
		req.Header.Set(padding.Header, padding.wireValue())
	}
	w := &WaitReadCloser{Wait: make(chan struct{}), cancel: cancel}
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			if !uploadOnly && requestCtx.Err() == nil {
				c.closed.Store(true)
			}
			notify(dialResult{err: err})
			common.Close(body)
			w.Fail(err)
			return
		}
		if resp.StatusCode != http.StatusOK {
			err := errors.New("XERA-HTTP stream rejected with HTTP status ", resp.StatusCode)
			notify(dialResult{err: err})
			resp.Body.Close()
			common.Close(body)
			w.Fail(err)
			return
		}
		if uploadOnly {
			if padding != nil && padding.Uplink && resp.Header.Get(padding.Header) != padding.wireValue() {
				resp.Body.Close()
				common.Close(body)
				w.Fail(errors.New("server did not confirm custom uplink padding"))
				return
			}
			_, readErr := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			common.Close(body)
			if readErr != nil {
				w.Fail(readErr)
			} else {
				w.Close()
			}
			return
		}
		if padding != nil {
			if resp.Header.Get(padding.Header) != padding.wireValue() {
				resp.Body.Close()
				common.Close(body)
				w.Fail(errors.New("server did not confirm custom downlink padding"))
				return
			}
			resp.Body = &paddedDownlinkReader{ReadCloser: resp.Body}
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
	padding := c.transportConfig.CustomDownlinkPadding
	if padding != nil && padding.Uplink {
		if c.transportConfig.GetNormalizedUplinkDataPlacement() != PlacementBody {
			buf.ReleaseMulti(payload)
			return errors.New("custom uplink padding requires body placement")
		}
		if err := padding.Validate(); err != nil {
			buf.ReleaseMulti(payload)
			return err
		}
	}
	method := c.transportConfig.GetNormalizedUplinkHTTPMethod()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		buf.ReleaseMulti(payload)
		return err
	}
	if err := c.transportConfig.FillPacketRequest(req, sessionId, seqStr, payload); err != nil {
		return err
	}
	if padding != nil && padding.Uplink {
		req.Header.Set(padding.Header, padding.wireValue())
		allowance, _ := ctx.Value(uploadPaddingKey{}).(*sharedPaddingAllowance)
		encoder, length := planPaddedPacket(req.Body, padding, allowance, req.ContentLength)
		req.Body = encoder
		req.ContentLength = length
		if length == 0 {
			encoder.Close()
			req.Body = http.NoBody
		}
		req.GetBody = nil
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
	if padding != nil && padding.Uplink && resp.Header.Get(padding.Header) != padding.wireValue() {
		return errors.New("server did not confirm custom uplink padding")
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (c *DefaultDialerClient) Close() error {
	c.closed.Store(true)
	c.client.CloseIdleConnections()
	if c.uploadClient != nil {
		c.uploadClient.CloseIdleConnections()
	}
	if h3Transport, ok := c.client.Transport.(*http3.Transport); ok {
		return h3Transport.Close()
	}
	return nil
}

type WaitReadCloser struct {
	Wait    chan struct{}
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
	close(w.Wait)
	w.mu.Unlock()
}

func (w *WaitReadCloser) Read(b []byte) (int, error) {
	<-w.Wait
	w.mu.Lock()
	rc := w.reader
	closed := w.closed
	readErr := w.readErr
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
		close(w.Wait)
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
