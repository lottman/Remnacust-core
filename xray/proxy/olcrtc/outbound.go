package olcrtc

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	olcclient "github.com/openlibrecommunity/olcrtc/pkg/olcrtc/client"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/proxy/socks"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
)

const readyTimeout = 90 * time.Second

type Handler struct {
	config    *Config
	cancel    context.CancelFunc
	ready     chan struct{}
	done      chan struct{}
	readyOnce sync.Once
	mu        sync.RWMutex
	address   string
	runErr    error
}

func New(ctx context.Context, config *Config) (*Handler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	h := &Handler{config: config, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	go h.run(runCtx)
	return h, nil
}

func (h *Handler) run(ctx context.Context) {
	local := olcclient.New(olcclient.Config{
		Transport:        h.config.Transport,
		Provider:         h.config.Provider,
		RoomURL:          h.config.RoomId,
		KeyHex:           h.config.CryptoKey,
		ProviderToken:    h.config.AuthToken,
		LocalAddr:        "127.0.0.1:0",
		DNSServer:        h.config.DnsServer,
		DeviceID:         h.config.DeviceId,
		TransportOptions: transportOptions(h.config.Transport),
		Liveness:         olcclient.LivenessConfig{Interval: 10 * time.Second, Timeout: 5 * time.Second, Failures: 3},
	})
	err := local.RunWithAddress(ctx, func(address string) {
		h.mu.Lock()
		h.address = address
		h.mu.Unlock()
		h.readyOnce.Do(func() { close(h.ready) })
	})
	h.mu.Lock()
	h.runErr = err
	h.mu.Unlock()
	close(h.done)
}

func transportOptions(name string) olcclient.TransportOptions {
	switch name {
	case "vp8channel":
		return olcclient.VP8Options{FPS: 30, BatchSize: 64}
	case "seichannel":
		return olcclient.SEIOptions{FPS: 30, BatchSize: 64, FragmentSize: 900, AckTimeoutMS: 2000}
	case "videochannel":
		return olcclient.VideoOptions{Width: 1080, Height: 1080, FPS: 30, QRRecovery: "low", Codec: "qrcode", TileModule: 4}
	default:
		return nil
	}
}

func (h *Handler) waitReady(ctx context.Context) error {
	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	select {
	case <-h.ready:
		// Readiness remains closed after shutdown; do not select a stale listener.
		select {
		case <-h.done:
			return h.error()
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	case <-h.done:
		return h.error()
	case <-timer.C:
		return errors.New("olcRTC did not become ready within 90 seconds")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) error() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.runErr == nil {
		return errors.New("olcRTC stopped before becoming ready")
	}
	return h.runErr
}

func (h *Handler) localAddress() (string, int, error) {
	h.mu.RLock()
	address := h.address
	h.mu.RUnlock()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, fmt.Errorf("invalid olcRTC listener address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid olcRTC listener port %q", portText)
	}
	return host, port, nil
}

func (h *Handler) Process(ctx context.Context, link *transport.Link, dialer internet.Dialer) error {
	outbounds := session.OutboundsFromContext(ctx)
	if len(outbounds) == 0 {
		return errors.New("olcRTC target is missing")
	}
	ob := outbounds[len(outbounds)-1]
	if !ob.Target.IsValid() {
		return errors.New("olcRTC target is invalid")
	}
	if ob.Target.Network != xnet.Network_TCP {
		return errors.New("olcRTC supports TCP CONNECT only")
	}
	ob.Name = "olcrtc"
	ob.CanSpliceCopy = 2
	if err := h.waitReady(ctx); err != nil {
		return errors.New("olcRTC is not ready").Base(err)
	}
	host, port, err := h.localAddress()
	if err != nil {
		return errors.New(err.Error())
	}
	conn, err := dialer.Dial(ctx, xnet.TCPDestination(xnet.DomainAddress(host), xnet.Port(port)))
	if err != nil {
		return errors.New("failed to connect to embedded olcRTC").Base(err)
	}
	defer func() { common.Close(conn) }()
	stopClose := context.AfterFunc(ctx, func() { common.Close(conn) })
	defer stopClose()
	request := &protocol.RequestHeader{Command: protocol.RequestCommandTCP, Address: ob.Target.Address, Port: ob.Target.Port}
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return errors.New("failed to set olcRTC handshake deadline").Base(err)
	}
	if _, err := socks.ClientHandshake(request, conn, conn); err != nil {
		return errors.New("failed to establish embedded olcRTC SOCKS session").Base(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return errors.New("failed to clear olcRTC handshake deadline").Base(err)
	}
	return task.Run(ctx,
		func() error { return buf.Copy(link.Reader, buf.NewWriter(conn)) },
		task.OnSuccess(func() error { return buf.Copy(buf.NewReader(conn), link.Writer) }, task.Close(link.Writer)),
	)
}

func (h *Handler) Close() error {
	h.cancel()
	return nil
}

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		return New(ctx, config.(*Config))
	}))
}
