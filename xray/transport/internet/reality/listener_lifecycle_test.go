package reality

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goreality "github.com/xtls/reality"
)

type realityTargetConn struct {
	*net.TCPConn
	closed    chan struct{}
	reading   chan struct{}
	closes    atomic.Int32
	closeOnce sync.Once
	readOnce  sync.Once
	closeHook func(int32)
}

func (c *realityTargetConn) Read(b []byte) (int, error) {
	c.readOnce.Do(func() { close(c.reading) })
	return c.TCPConn.Read(b)
}

func (c *realityTargetConn) Close() error {
	count := c.closes.Add(1)
	if c.closeHook != nil {
		c.closeHook(count)
	}
	err := c.TCPConn.Close()
	c.closeOnce.Do(func() { close(c.closed) })
	return err
}

type realityTargetFixture struct {
	listener net.Listener
	peers    chan *net.TCPConn
	dials    chan *realityTargetConn
	done     chan struct{}
	mu       sync.Mutex
	conns    []net.Conn
	close    sync.Once
}

func newRealityTargetFixture(t *testing.T) *realityTargetFixture {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &realityTargetFixture{listener: listener, peers: make(chan *net.TCPConn, 32), dials: make(chan *realityTargetConn, 32), done: make(chan struct{})}
	go func() {
		defer close(f.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, conn)
			f.mu.Unlock()
			f.peers <- conn.(*net.TCPConn)
		}
	}()
	t.Cleanup(f.Close)
	return f
}

func (f *realityTargetFixture) Close() {
	f.close.Do(func() {
		_ = f.listener.Close()
		<-f.done
		f.mu.Lock()
		conns := append([]net.Conn(nil), f.conns...)
		f.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
}

func (f *realityTargetFixture) config(hook func(int32)) *goreality.Config {
	return &goreality.Config{
		Type: "tcp", Dest: f.listener.Addr().String(),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			target := &realityTargetConn{TCPConn: conn.(*net.TCPConn), closed: make(chan struct{}), reading: make(chan struct{}), closeHook: hook}
			f.mu.Lock()
			f.conns = append(f.conns, target)
			f.mu.Unlock()
			f.dials <- target
			return target, nil
		},
	}
}

func realityLifecycleReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("loopback fixture timed out")
		var zero T
		return zero
	}
}

func realityLifecycleWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	realityLifecycleReceive(t, ch)
}

func realityLifecycleListener(t *testing.T, ctx context.Context, cfg *goreality.Config, target *realityTargetFixture) *ownedListener {
	t.Helper()
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	listener := NewListener(ctx, inner, cfg).(*ownedListener)
	t.Cleanup(func() {
		_ = listener.Close()
		if target != nil {
			target.Close()
		}
		realityLifecycleDrained(t, listener)
	})
	return listener
}

func realityLifecycleDrained(t *testing.T, listener *ownedListener) {
	t.Helper()
	done := make(chan struct{})
	go func() { listener.handshakes.Wait(); close(done) }()
	realityLifecycleWait(t, done)
	listener.mu.Lock()
	pending := len(listener.pending)
	listener.mu.Unlock()
	if pending != 0 {
		t.Fatalf("retained %d completed handshakes", pending)
	}
}

func realityLifecycleDial(t *testing.T, listener net.Listener) *net.TCPConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*net.TCPConn)
}

func realityLifecycleFIN(t *testing.T, client, peer *net.TCPConn) {
	t.Helper()
	request := []byte("invalid-test-hello")
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	data := make([]byte, len(request))
	if _, err := io.ReadFull(peer, data); err != nil || string(data) != string(request) {
		t.Fatalf("forwarded request %q: %v", data, err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if n, err := peer.Read(data[:1]); n != 0 || err != io.EOF {
		t.Fatalf("forwarded FIN: n=%d err=%v", n, err)
	}
	_ = peer.SetReadDeadline(time.Time{})
}

func TestRealityListenerClosesPendingTargetAfterFIN(t *testing.T) {
	f := newRealityTargetFixture(t)
	listener := realityLifecycleListener(t, context.Background(), f.config(nil), f)
	client := realityLifecycleDial(t, listener)
	target := realityLifecycleReceive(t, f.dials)
	peer := realityLifecycleReceive(t, f.peers)
	realityLifecycleWait(t, target.reading)
	realityLifecycleFIN(t, client, peer)
	_ = listener.Close()
	realityLifecycleWait(t, target.closed)
	realityLifecycleDrained(t, listener)
}

func TestRealityListenerCancellationClosesPendingTarget(t *testing.T) {
	f := newRealityTargetFixture(t)
	listener := realityLifecycleListener(t, context.Background(), f.config(nil), f)
	client := realityLifecycleDial(t, listener)
	target := realityLifecycleReceive(t, f.dials)
	realityLifecycleWait(t, target.reading)
	listener.cancel()
	realityLifecycleWait(t, target.closed)
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("canceled target left client usable: n=%d err=%v", n, err)
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("canceled target retained client", err)
	}
	realityLifecycleDrained(t, listener)
}

func TestRealityListenerResetClosesPendingTarget(t *testing.T) {
	f := newRealityTargetFixture(t)
	listener := realityLifecycleListener(t, context.Background(), f.config(nil), f)
	client := realityLifecycleDial(t, listener)
	target := realityLifecycleReceive(t, f.dials)
	peer := realityLifecycleReceive(t, f.peers)
	realityLifecycleWait(t, target.reading)
	request := []byte("invalid-test-hello")
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(peer, make([]byte, len(request))); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Time{})
	if err := client.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	realityLifecycleWait(t, target.closed)
	realityLifecycleDrained(t, listener)
}

func TestRealityListenerConcurrentHandshakesKeepConfigImmutable(t *testing.T) {
	f := newRealityTargetFixture(t)
	cfg := f.config(nil)
	dial := reflect.ValueOf(cfg.DialContext).Pointer()
	listener := realityLifecycleListener(t, context.Background(), cfg, f)
	for range 16 {
		realityLifecycleDial(t, listener)
	}
	targets := make([]*realityTargetConn, 16)
	for i := range targets {
		targets[i] = realityLifecycleReceive(t, f.dials)
		realityLifecycleWait(t, targets[i].reading)
	}
	if got := reflect.ValueOf(cfg.DialContext).Pointer(); got != dial {
		t.Fatal("handshakes changed the shared REALITY config")
	}
	_ = listener.Close()
	for _, target := range targets {
		realityLifecycleWait(t, target.closed)
	}
	realityLifecycleDrained(t, listener)
}

func TestRealityListenerPreservesDelayedFallbackAfterFIN(t *testing.T) {
	f := newRealityTargetFixture(t)
	listener := realityLifecycleListener(t, context.Background(), f.config(nil), f)
	client := realityLifecycleDial(t, listener)
	target := realityLifecycleReceive(t, f.dials)
	peer := realityLifecycleReceive(t, f.peers)
	realityLifecycleWait(t, target.reading)
	realityLifecycleFIN(t, client, peer)
	select {
	case <-target.closed:
		t.Fatal("fallback target closed before its delayed response")
	case <-time.After(350 * time.Millisecond):
	}
	if _, err := peer.Write([]byte("delayed-response")); err != nil {
		t.Fatal(err)
	}
	if err := peer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	response, err := io.ReadAll(client)
	if err != nil || string(response) != "delayed-response" {
		t.Fatalf("fallback response %q: %v", response, err)
	}
	realityLifecycleWait(t, target.closed)
	realityLifecycleDrained(t, listener)
	closes := target.closes.Load()
	_ = listener.Close()
	time.Sleep(25 * time.Millisecond)
	if got := target.closes.Load(); got != closes {
		t.Fatalf("completed target retained a cancellation callback: closes %d -> %d", closes, got)
	}
}

func TestRealityListenerNilContext(t *testing.T) {
	cfg := &goreality.Config{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("fixture dial failure")
	}}
	listener := realityLifecycleListener(t, nil, cfg, nil)
	client := realityLifecycleDial(t, listener)
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("failed dial left client usable: n=%d err=%v", n, err)
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("failed dial retained client", err)
	}
	_ = listener.Close()
	realityLifecycleDrained(t, listener)
}

func TestRealityListenerCloseCancelsDial(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	cfg := &goreality.Config{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}}
	listener := realityLifecycleListener(t, context.Background(), cfg, nil)
	realityLifecycleDial(t, listener)
	realityLifecycleWait(t, started)
	var closing sync.WaitGroup
	for range 16 {
		closing.Go(func() { _ = listener.Close() })
	}
	closing.Wait()
	realityLifecycleWait(t, canceled)
	realityLifecycleDrained(t, listener)
}

func TestRealityListenerClosesFailedDialTarget(t *testing.T) {
	f := newRealityTargetFixture(t)
	cfg := f.config(nil)
	dial := cfg.DialContext
	cfg.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return target, errors.New("fixture failed after allocating target")
	}
	listener := realityLifecycleListener(t, context.Background(), cfg, f)
	realityLifecycleDial(t, listener)
	target := realityLifecycleReceive(t, f.dials)
	realityLifecycleWait(t, target.closed)
	_ = listener.Close()
	realityLifecycleDrained(t, listener)
}

func TestRealityListenerCloseRacesLateDial(t *testing.T) {
	for range 12 {
		f := newRealityTargetFixture(t)
		cfg := f.config(nil)
		dial := cfg.DialContext
		started, release := make(chan struct{}), make(chan struct{})
		var released sync.Once
		cfg.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			target, err := dial(ctx, network, address)
			close(started)
			<-release
			return target, err
		}
		listener := realityLifecycleListener(t, context.Background(), cfg, f)
		t.Cleanup(func() { released.Do(func() { close(release) }) })
		realityLifecycleDial(t, listener)
		target := realityLifecycleReceive(t, f.dials)
		realityLifecycleWait(t, started)
		_ = listener.Close()
		released.Do(func() { close(release) })
		realityLifecycleWait(t, target.closed)
		realityLifecycleDrained(t, listener)
	}
}

func TestRealityListenerTargetCancellationRacesServerReturn(t *testing.T) {
	for range 12 {
		started, release := make(chan struct{}), make(chan struct{})
		var released sync.Once
		f := newRealityTargetFixture(t)
		cfg := f.config(func(count int32) {
			if count == 1 {
				close(started)
				<-release
			}
		})
		listener := realityLifecycleListener(t, context.Background(), cfg, f)
		t.Cleanup(func() { released.Do(func() { close(release) }) })
		client := realityLifecycleDial(t, listener)
		target := realityLifecycleReceive(t, f.dials)
		peer := realityLifecycleReceive(t, f.peers)
		realityLifecycleWait(t, target.reading)
		realityLifecycleFIN(t, client, peer)
		_ = listener.Close()
		realityLifecycleWait(t, started)
		_ = peer.Close()
		realityLifecycleDrained(t, listener)
		released.Do(func() { close(release) })
		realityLifecycleWait(t, target.closed)
	}
}
