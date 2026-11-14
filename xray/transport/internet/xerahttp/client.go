package xerahttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

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
	adaptive        *adaptiveController
	passiveRTT      func() time.Duration
	releaseRTT      func()
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
	mask := adaptiveDown
	if uploadOnly {
		mask = adaptiveUp
	} else if body != nil {
		mask |= adaptiveUp
	}
	lease, err := c.adaptive.acquire(ctx, mask)
	if err != nil {
		common.Close(body)
		return nil, nil, nil, err
	}
	if lease != nil {
		lease.passiveRTT = c.passiveRTT
	}
	handedOff := false
	defer func() {
		if !handedOff {
			if lease != nil {
				common.Close(body)
			}
			lease.finish(nil)
		}
	}()
	type dialResult struct {
		remote net.Addr
		local  net.Addr
		err    error
	}
	connected := make(chan dialResult, 1)
	var notifyOnce sync.Once
	notify := func(result dialResult) { notifyOnce.Do(func() { connected <- result }) }
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if uploadOnly {
		requestCtx = context.WithValue(requestCtx, browserH2UploadKey{}, true)
	}
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
		if lease != nil {
			lease.requestBodyPending.Store(true)
			closer, ok := body.(io.ReadCloser)
			if !ok {
				closer = io.NopCloser(body)
			}
			body = &adaptiveReader{ReadCloser: closer, lease: lease, direction: adaptiveUp, requestBody: true}
		}
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
	c.transportConfig.applyFetchOriginHeaders(req)
	padding := c.transportConfig.CustomDownlinkPadding
	if padding != nil {
		req.Header.Set(padding.Header, padding.wireValue())
	}
	w := &WaitReadCloser{Wait: make(chan struct{}), cancel: cancel}
	handedOff = true
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			failure := err
			if requestCtx.Err() != nil {
				failure = nil
			}
			lease.finish(failure)
			if !uploadOnly && requestCtx.Err() == nil {
				c.closed.Store(true)
			}
			notify(dialResult{err: err})
			common.Close(body)
			w.Fail(err)
			return
		}
		if resp.StatusCode != http.StatusOK {
			lease.reject(resp.StatusCode)
			err := errors.New("XERA-HTTP stream rejected with HTTP status ", resp.StatusCode)
			notify(dialResult{err: err})
			resp.Body.Close()
			common.Close(body)
			lease.finish(nil)
			w.Fail(err)
			return
		}
		if uploadOnly {
			if padding != nil && !padding.matchesHeader(resp.Header) {
				lease.reject(0)
				resp.Body.Close()
				common.Close(body)
				lease.finish(nil)
				w.Fail(errors.New("server did not confirm custom uplink padding"))
				return
			}
			lease.confirm()
			_, readErr := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			common.Close(body)
			failure := readErr
			if requestCtx.Err() != nil {
				failure = nil
			}
			lease.endResponse(failure)
			if readErr != nil {
				w.Fail(readErr)
			} else {
				w.Close()
			}
			return
		}
		if padding != nil {
			if !padding.matchesHeader(resp.Header) {
				lease.reject(0)
				resp.Body.Close()
				common.Close(body)
				lease.finish(nil)
				w.Fail(errors.New("server did not confirm custom downlink padding"))
				return
			}
			resp.Body = &paddedDownlinkReader{ReadCloser: resp.Body}
		}
		lease.confirm()
		if lease != nil {
			resp.Body = &adaptiveReader{ReadCloser: resp.Body, lease: lease, direction: adaptiveDown, release: true, ctx: requestCtx, onClose: func() { common.Close(body) }}
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
func (c *DefaultDialerClient) PostPacket(ctx context.Context, url string, sessionId string, seqStr string, payload buf.MultiBuffer) (result error) {
	ctx = context.WithValue(ctx, browserH2UploadKey{}, true)
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
	lease, err := c.adaptive.acquire(ctx, adaptiveUp)
	if err != nil {
		buf.ReleaseMulti(payload)
		return err
	}
	if lease != nil {
		lease.passiveRTT = c.passiveRTT
	}
	defer func() {
		failure := result
		if ctx.Err() != nil {
			failure = nil
		}
		lease.finish(failure)
	}()
	payloadBytes := int(payload.Len())
	method := c.transportConfig.GetNormalizedUplinkHTTPMethod()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		buf.ReleaseMulti(payload)
		return err
	}
	if err := c.transportConfig.FillPacketRequest(req, sessionId, seqStr, payload); err != nil {
		return err
	}
	c.transportConfig.applyFetchOriginHeaders(req)
	if padding != nil {
		req.Header.Set(padding.Header, padding.wireValue())
	}
	if padding != nil && padding.Uplink {
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
	if lease != nil && req.Body != nil {
		req.Body = &adaptiveReader{ReadCloser: req.Body}
		defer req.Body.Close()
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
		lease.reject(resp.StatusCode)
		return errors.New("bad status code:", resp.Status)
	}
	if padding != nil && !padding.matchesHeader(resp.Header) {
		lease.reject(0)
		return errors.New("server did not confirm custom padding")
	}
	lease.confirm()
	lease.progress(adaptiveUp, payloadBytes)
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (c *DefaultDialerClient) Close() error {
	if c.releaseRTT != nil {
		defer c.releaseRTT()
	}
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
