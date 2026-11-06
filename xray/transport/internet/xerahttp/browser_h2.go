package xerahttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttp2 "github.com/bogdanfinn/fhttp/http2"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tls"
)

const maxBrowserH2Connections = 4
const maxBrowserH2Requests = 32

type browserH2Connection struct {
	wire   *fhttp2.ClientConn
	raw    net.Conn
	active int
	used   bool
}

type browserH2UploadKey struct{}

type browserH2Transport struct {
	wire      *fhttp2.Transport
	dial      func(context.Context) (net.Conn, error)
	mu        sync.Mutex
	conns     []*browserH2Connection
	wake      chan struct{}
	closed    bool
	dialing   bool
	authority string
	lifetime  context.Context
	cancel    context.CancelFunc
	active    [2]int
}

func usesChrome133HTTP2(settings *internet.MemoryStreamConfig) bool {
	name := ""
	if config := tls.ConfigFromStreamSettings(settings); config != nil {
		name = config.Fingerprint
	}
	if config := reality.ConfigFromStreamSettings(settings); config != nil {
		name = config.Fingerprint
	}
	fingerprint := tls.GetFingerprint(name)
	return fingerprint != nil && strings.EqualFold(fingerprint.Client, "Chrome") && fingerprint.Version == "133"
}

func newBrowserH2Transport(dial func(context.Context) (net.Conn, error), keepAlive time.Duration) *browserH2Transport {
	lifetime, cancel := context.WithCancel(context.Background())
	return &browserH2Transport{
		dial: dial, wake: make(chan struct{}), lifetime: lifetime, cancel: cancel,
		wire: &fhttp2.Transport{
			DisableCompression: true,
			Settings: map[fhttp2.SettingID]uint32{
				fhttp2.SettingHeaderTableSize: 65536, fhttp2.SettingEnablePush: 0,
				fhttp2.SettingInitialWindowSize: 6291456, fhttp2.SettingMaxHeaderListSize: 262144,
			},
			SettingsOrder:     []fhttp2.SettingID{fhttp2.SettingHeaderTableSize, fhttp2.SettingEnablePush, fhttp2.SettingInitialWindowSize, fhttp2.SettingMaxHeaderListSize},
			PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},
			ConnectionFlow:    15663105, IdleConnTimeout: xnet.ConnIdleTimeout, ReadIdleTimeout: keepAlive,
		},
	}
}

func (t *browserH2Transport) signalLocked() {
	close(t.wake)
	t.wake = make(chan struct{})
}

func (t *browserH2Transport) acquire(ctx context.Context, authority string, direction int) (*browserH2Connection, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		t.mu.Lock()
		if t.closed || t.authority != "" && t.authority != authority {
			t.mu.Unlock()
			return nil, false, errors.New("Xera HTTP/2 transport is closed or authority changed")
		}
		t.authority = authority
		if t.active[direction] >= maxBrowserH2Requests {
			wake := t.wake
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-wake:
				continue
			}
		}
		for i := 0; i < len(t.conns); {
			entry := t.conns[i]
			if entry.wire.CanTakeNewRequest() {
				reused := entry.used
				entry.used = true
				entry.active++
				t.active[direction]++
				t.mu.Unlock()
				return entry, reused, nil
			}
			if entry.active == 0 {
				entry.wire.Close()
				t.conns = append(t.conns[:i], t.conns[i+1:]...)
				continue
			}
			i++
		}
		if !t.dialing && len(t.conns) < maxBrowserH2Connections {
			t.dialing = true
			t.active[direction]++
			t.mu.Unlock()
			dialCtx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(t.lifetime, cancel)
			raw, err := t.dial(dialCtx)
			var wire *fhttp2.ClientConn
			if err == nil {
				stopClose := context.AfterFunc(dialCtx, func() { raw.Close() })
				raw.SetWriteDeadline(time.Now().Add(16 * time.Second))
				wire, err = t.wire.NewClientConn(newBrowserH2Guard(raw))
				raw.SetWriteDeadline(time.Time{})
				stopClose()
				if dialCtx.Err() != nil {
					err = dialCtx.Err()
				}
			}
			stop()
			cancel()
			t.mu.Lock()
			t.dialing = false
			if t.closed && err == nil {
				err = io.ErrClosedPipe
			}
			if err != nil {
				t.active[direction]--
				if raw != nil {
					raw.Close()
				}
				t.signalLocked()
				t.mu.Unlock()
				return nil, false, err
			}
			entry := &browserH2Connection{wire: wire, raw: raw, active: 1, used: true}
			t.conns = append(t.conns, entry)
			t.signalLocked()
			t.mu.Unlock()
			return entry, false, nil
		}
		if !t.dialing && len(t.conns) == maxBrowserH2Connections {
			t.mu.Unlock()
			return nil, false, errors.New("Xera HTTP/2 connection capacity exhausted")
		}
		wake := t.wake
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-wake:
		}
	}
}

func (t *browserH2Transport) release(entry *browserH2Connection, direction int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry.active--
	t.active[direction]--
	if t.closed && entry.active == 0 {
		entry.wire.Close()
	}
	t.signalLocked()
}

func (t *browserH2Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL == nil || request.URL.Scheme != "https" {
		if request.Body != nil {
			request.Body.Close()
		}
		return nil, errors.New("browser HTTP/2 requires HTTPS")
	}
	trace := httptrace.ContextClientTrace(request.Context())
	if trace != nil && trace.GetConn != nil {
		trace.GetConn(request.URL.Host)
	}
	direction := 0
	if upload, _ := request.Context().Value(browserH2UploadKey{}).(bool); upload {
		direction = 1
	}
	entry, reused, err := t.acquire(request.Context(), request.URL.Host, direction)
	if err != nil {
		if request.Body != nil {
			request.Body.Close()
		}
		return nil, err
	}
	if trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{Conn: entry.raw, Reused: reused})
	}
	wireRequest, err := fhttp.NewRequestWithContext(request.Context(), request.Method, request.URL.String(), request.Body)
	if err != nil {
		t.release(entry, direction)
		if request.Body != nil {
			request.Body.Close()
		}
		return nil, err
	}
	wireRequest.Host = request.Host
	wireRequest.ContentLength = request.ContentLength
	wireRequest.Header = fhttp.Header(request.Header.Clone())
	delete(wireRequest.Header, fhttp.PHeaderOrderKey)
	wireRequest.Header[fhttp.HeaderOrderKey] = browserH2HeaderOrder(request.Header)
	response, err := entry.wire.RoundTrip(wireRequest)
	if err != nil {
		t.release(entry, direction)
		return nil, err
	}
	body := &browserH2Body{ReadCloser: response.Body, release: func() { t.release(entry, direction) }}
	return &http.Response{
		Status: response.Status, StatusCode: response.StatusCode, Proto: response.Proto,
		ProtoMajor: response.ProtoMajor, ProtoMinor: response.ProtoMinor,
		Header: http.Header(response.Header), Body: body, ContentLength: response.ContentLength,
		TransferEncoding: response.TransferEncoding, Trailer: http.Header(response.Trailer), Request: request,
	}, nil
}

func browserH2HeaderOrder(header http.Header) []string {
	order := []string{"content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua", "content-type", "sec-ch-ua-mobile", "accept", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding", "accept-language", "priority", "cookie"}
	seen := make(map[string]bool, len(order))
	for _, name := range order {
		seen[name] = true
	}
	var extra []string
	for name := range header {
		name = strings.ToLower(name)
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return append(order, extra...)
}

func (t *browserH2Transport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.cancel()
	for _, entry := range t.conns {
		if entry.active == 0 {
			entry.wire.Close()
		}
	}
	t.signalLocked()
}

type browserH2Body struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *browserH2Body) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *browserH2Body) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
