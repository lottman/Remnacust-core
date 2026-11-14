package outbound

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/pipe"
)

type rejectPolicy struct {
	policy.DefaultManager
	idle time.Duration
}

func (p rejectPolicy) ForLevel(uint32) policy.Session {
	s := policy.SessionDefault()
	s.Timeouts.ConnectionIdle = p.idle
	return s
}

type rejectConn struct {
	read    *bytes.Reader
	written bytes.Buffer
	mu      sync.Mutex
}

func (c *rejectConn) Read(p []byte) (int, error) { return c.read.Read(p) }
func (c *rejectConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(p)
}
func (*rejectConn) Close() error                     { return nil }
func (*rejectConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*rejectConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*rejectConn) SetDeadline(time.Time) error      { return nil }
func (*rejectConn) SetReadDeadline(time.Time) error  { return nil }
func (*rejectConn) SetWriteDeadline(time.Time) error { return nil }

type rejectDialer struct {
	calls atomic.Int32
	conn  *rejectConn
}

func (d *rejectDialer) Dial(context.Context, net.Destination) (stat.Connection, error) {
	d.calls.Add(1)
	return d.conn, nil
}
func (*rejectDialer) DestIpAddress() net.IP                                 { return nil }
func (*rejectDialer) SetOutboundGateway(context.Context, *session.Outbound) {}

type rejectReader struct {
	*pipe.Reader
	bytes       atomic.Int64
	reads       chan struct{}
	interrupted atomic.Bool
}

func (r *rejectReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if !mb.IsEmpty() {
		r.bytes.Add(int64(mb.Len()))
		select {
		case r.reads <- struct{}{}:
		default:
		}
	}
	return mb, err
}

func (r *rejectReader) Interrupt() {
	r.interrupted.Store(true)
	r.Reader.Interrupt()
}

func rejectFixture(t *testing.T, flow string, port net.Port, idle time.Duration) (*Handler, context.Context, context.CancelFunc, *transport.Link, *pipe.Writer, *rejectReader, *rejectDialer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx = session.ContextWithOutbounds(ctx, []*session.Outbound{{Target: net.UDPDestination(net.LocalHostIP, port)}})
	h := &Handler{
		server: protocol.NewServerSpec(net.TCPDestination(net.LocalHostIP, 12345), &protocol.MemoryUser{
			Account: &vless.MemoryAccount{ID: protocol.NewID(uuid.UUID{1}), Flow: flow},
		}),
		policyManager: rejectPolicy{idle: idle},
	}
	reader, writer := pipe.New(pipe.WithoutSizeLimit())
	tracked := &rejectReader{Reader: reader, reads: make(chan struct{}, 16)}
	t.Cleanup(func() { reader.Interrupt() })
	dialer := &rejectDialer{conn: &rejectConn{read: bytes.NewReader([]byte{0, 0})}}
	return h, ctx, cancel, &transport.Link{Reader: tracked, Writer: buf.Discard}, writer, tracked, dialer
}

func waitRejected(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "XTLS rejected UDP/443 traffic") {
			t.Fatalf("unexpected rejection result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rejected flow did not finish")
	}
}

func writeRejectedPacket(t *testing.T, writer *pipe.Writer, reader *rejectReader) {
	t.Helper()
	packet := buf.New()
	if _, err := packet.Write([]byte{0xc0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteMultiBuffer(buf.MultiBuffer{packet}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reader.reads:
	case <-time.After(time.Second):
		t.Fatal("rejected flow stopped draining packets")
	}
}

func TestVisionUDP443RejectRetainsAndDrainsUntilCancel(t *testing.T) {
	h, ctx, cancel, link, writer, reader, dialer := rejectFixture(t, vless.XRV, 443, time.Second)
	done := make(chan error, 1)
	go func() { done <- h.Process(ctx, link, dialer) }()
	for range 3 {
		writeRejectedPacket(t, writer, reader)
		select {
		case err := <-done:
			t.Fatalf("rejected association ended before cancellation: %v", err)
		default:
		}
	}
	cancel()
	waitRejected(t, done)
	if dialer.calls.Load() != 0 || reader.bytes.Load() != 12 || !reader.interrupted.Load() {
		t.Fatalf("dialed=%d drained=%d interrupted=%t", dialer.calls.Load(), reader.bytes.Load(), reader.interrupted.Load())
	}
}

func TestVisionUDP443RejectPolicyIdleAndActivity(t *testing.T) {
	const idle = 250 * time.Millisecond
	h, ctx, _, link, writer, reader, dialer := rejectFixture(t, vless.XRV, 443, idle)
	done := make(chan error, 1)
	go func() { done <- h.Process(ctx, link, dialer) }()
	for range 8 {
		writeRejectedPacket(t, writer, reader)
		time.Sleep(idle / 4)
		select {
		case err := <-done:
			t.Fatalf("active rejected association expired: %v", err)
		default:
		}
	}
	waitRejected(t, done)
	if dialer.calls.Load() != 0 || !reader.interrupted.Load() {
		t.Fatalf("dialed=%d interrupted=%t", dialer.calls.Load(), reader.interrupted.Load())
	}
}

func TestVisionUDP443RejectWrappedReaderCancel(t *testing.T) {
	h, ctx, cancel, link, writer, reader, dialer := rejectFixture(t, vless.XRV, 443, time.Second)
	link.Reader = &buf.EndpointOverrideReader{Reader: &buf.TimeoutWrapperReader{Reader: reader}}
	done := make(chan error, 1)
	go func() { done <- h.Process(ctx, link, dialer) }()
	writeRejectedPacket(t, writer, reader)
	cancel()
	waitRejected(t, done)
	if !reader.interrupted.Load() || dialer.calls.Load() != 0 {
		t.Fatalf("interrupted=%t dialed=%d", reader.interrupted.Load(), dialer.calls.Load())
	}
}

func TestVisionUDP443RejectDeadlineAndZeroIdle(t *testing.T) {
	for _, idle := range []time.Duration{0, time.Second} {
		t.Run(idle.String(), func(t *testing.T) {
			h, ctx, _, link, _, reader, dialer := rejectFixture(t, vless.XRV, 443, idle)
			ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- h.Process(ctx, link, dialer) }()
			waitRejected(t, done)
			if !reader.interrupted.Load() || dialer.calls.Load() != 0 {
				t.Fatalf("interrupted=%t dialed=%d", reader.interrupted.Load(), dialer.calls.Load())
			}
		})
	}
}

func TestVisionUDP443RejectEOFAndAlreadyCancelled(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "eof"}[eof], func(t *testing.T) {
			h, ctx, cancel, link, writer, _, dialer := rejectFixture(t, vless.XRV, 443, time.Second)
			if eof {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			done := make(chan error, 1)
			go func() { done <- h.Process(ctx, link, dialer) }()
			waitRejected(t, done)
			if dialer.calls.Load() != 0 {
				t.Fatal("rejected flow dialed a server")
			}
		})
	}
}

func TestVisionUDP443PositiveControls(t *testing.T) {
	for _, control := range []struct {
		name string
		flow string
		port net.Port
	}{
		{name: "empty-flow", port: 443},
		{name: "vision-udp443", flow: vless.XRV + "-udp443", port: 443},
		{name: "vision-other-port", flow: vless.XRV, port: 53},
	} {
		t.Run(control.name, func(t *testing.T) {
			h, ctx, _, link, writer, _, dialer := rejectFixture(t, control.flow, control.port, time.Second)
			packet := buf.New()
			packet.Write([]byte{1, 2, 3, 4})
			if err := writer.WriteMultiBuffer(buf.MultiBuffer{packet}); err != nil {
				t.Fatal(err)
			}
			writer.Close()
			if err := h.Process(ctx, link, dialer); err != nil && err != io.EOF {
				t.Fatal(err)
			}
			if dialer.calls.Load() != 1 {
				t.Fatalf("dial count: %d", dialer.calls.Load())
			}
			dialer.conn.mu.Lock()
			size := dialer.conn.written.Len()
			dialer.conn.mu.Unlock()
			if size == 0 {
				t.Fatal("allowed flow did not send a VLESS request")
			}
		})
	}
}
