package reality

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

type retryAcceptResult struct {
	conn net.Conn
	err  error
}

type retryAcceptListener struct {
	calls   chan time.Time
	results chan retryAcceptResult
	done    chan struct{}
	closes  atomic.Int32
	once    sync.Once
}

func newRetryAcceptListener() *retryAcceptListener {
	return &retryAcceptListener{calls: make(chan time.Time, 16), results: make(chan retryAcceptResult), done: make(chan struct{})}
}

func (l *retryAcceptListener) Accept() (net.Conn, error) {
	l.calls <- time.Now()
	select {
	case result := <-l.results:
		return result.conn, result.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *retryAcceptListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (l *retryAcceptListener) Close() error {
	l.closes.Add(1)
	l.once.Do(func() { close(l.done) })
	return nil
}

func temporaryAcceptError() error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
}

func acceptRetryAfter(t *testing.T, listener *ownedListener, inner *retryAcceptListener, previous time.Time, delay time.Duration) time.Time {
	t.Helper()
	inner.results <- retryAcceptResult{err: temporaryAcceptError()}
	synctest.Wait()
	if inner.closes.Load() != 0 {
		t.Fatal("temporary Accept error permanently closed the listener")
	}
	listener.mu.Lock()
	pending := len(listener.pending)
	listener.mu.Unlock()
	if len(listener.slots) != pending {
		t.Fatal("failed Accept retained a handshake admission slot during backoff")
	}
	time.Sleep(delay - time.Nanosecond)
	synctest.Wait()
	select {
	case got := <-inner.calls:
		t.Fatalf("retry ran too early: elapsed=%v want=%v", got.Sub(previous), delay)
	default:
	}
	time.Sleep(time.Nanosecond)
	synctest.Wait()
	select {
	case got := <-inner.calls:
		if got.Sub(previous) != delay {
			t.Fatalf("retry delay=%v want=%v", got.Sub(previous), delay)
		}
		return got
	default:
		t.Fatalf("retry did not run after %v", delay)
		return time.Time{}
	}
}

func assertRetryTransferredConnection(t *testing.T, conn, peer net.Conn) {
	t.Helper()
	written := make(chan error, 1)
	go func() { _, err := peer.Write([]byte{'!'}); written <- err }()
	var data [1]byte
	if _, err := io.ReadFull(conn, data[:]); err != nil || data[0] != '!' {
		t.Fatalf("transferred connection was interrupted: %q %v", data, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func TestOwnedRealityListenerAcceptBackoffAndReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := newRetryAcceptListener()
		listener := newOwnedListener(context.Background(), inner, func(_ context.Context, raw net.Conn) (net.Conn, error) { return raw, nil })
		defer listener.Close()
		previous := <-inner.calls
		for _, delay := range []time.Duration{5, 10, 20, 40, 80, 160, 320, 640, 1000, 1000} {
			previous = acceptRetryAfter(t, listener, inner, previous, delay*time.Millisecond)
		}
		server, peer := net.Pipe()
		defer server.Close()
		defer peer.Close()
		inner.results <- retryAcceptResult{conn: server}
		transferred, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		previous = <-inner.calls
		acceptRetryAfter(t, listener, inner, previous, 5*time.Millisecond)
		listener.Close()
		synctest.Wait()
		assertRetryTransferredConnection(t, transferred, peer)
		if inner.closes.Load() != 1 {
			t.Fatalf("underlying listener closed %d times", inner.closes.Load())
		}
	})
}

func TestOwnedRealityListenerCloseInterruptsAcceptBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := newRetryAcceptListener()
		listener := newOwnedListener(context.Background(), inner, func(_ context.Context, raw net.Conn) (net.Conn, error) { return raw, nil })
		defer listener.Close()
		previous := <-inner.calls
		for _, delay := range []time.Duration{5, 10, 20, 40, 80, 160, 320, 640} {
			previous = acceptRetryAfter(t, listener, inner, previous, delay*time.Millisecond)
		}
		inner.results <- retryAcceptResult{err: temporaryAcceptError()}
		synctest.Wait()
		if inner.closes.Load() != 0 {
			t.Fatal("temporary error stopped the listener before Close")
		}
		before := time.Now()
		listener.Close()
		synctest.Wait()
		if elapsed := time.Since(before); elapsed != 0 {
			t.Fatalf("Close waited for the backoff timer: %v", elapsed)
		}
		if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close: %v", err)
		}
		select {
		case <-inner.calls:
			t.Fatal("Close left an Accept retry running")
		default:
		}
		if len(listener.slots) != 0 || inner.closes.Load() != 1 {
			t.Fatalf("Close retained admission or repeated cleanup: slots=%d closes=%d", len(listener.slots), inner.closes.Load())
		}
	})
}

func TestOwnedRealityListenerPermanentAcceptErrorCleansPending(t *testing.T) {
	for _, want := range []error{errors.New("permanent accept failure"), net.ErrClosed} {
		t.Run(want.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				inner := newRetryAcceptListener()
				active, activePeer := net.Pipe()
				defer active.Close()
				defer activePeer.Close()
				pending, pendingPeer := net.Pipe()
				defer pending.Close()
				defer pendingPeer.Close()
				started := make(chan struct{})
				listener := newOwnedListener(context.Background(), inner, func(ctx context.Context, raw net.Conn) (net.Conn, error) {
					if raw == active {
						return raw, nil
					}
					close(started)
					<-ctx.Done()
					return nil, ctx.Err()
				})
				defer listener.Close()
				<-inner.calls
				inner.results <- retryAcceptResult{conn: active}
				transferred, err := listener.Accept()
				if err != nil {
					t.Fatal(err)
				}
				<-inner.calls
				inner.results <- retryAcceptResult{conn: pending}
				<-started
				previous := <-inner.calls
				acceptRetryAfter(t, listener, inner, previous, 5*time.Millisecond)
				inner.results <- retryAcceptResult{err: fmt.Errorf("accept failed: %w", want)}
				if conn, err := listener.Accept(); conn != nil || !errors.Is(err, want) {
					t.Fatalf("permanent error changed: conn=%v err=%v", conn, err)
				}
				listener.handshakes.Wait()
				synctest.Wait()
				if len(listener.pending) != 0 || len(listener.slots) != 0 || inner.closes.Load() != 1 {
					t.Fatalf("permanent failure retained resources: pending=%d slots=%d closes=%d", len(listener.pending), len(listener.slots), inner.closes.Load())
				}
				if _, err := pendingPeer.Write([]byte{'?'}); err == nil {
					t.Fatal("permanent failure left the pending connection open")
				}
				assertRetryTransferredConnection(t, transferred, activePeer)
			})
		})
	}
}

type retryOnceTCPListener struct {
	net.Listener
	err   error
	calls atomic.Int32
}

func (l *retryOnceTCPListener) Accept() (net.Conn, error) {
	if l.calls.Add(1) == 1 {
		return nil, l.err
	}
	return l.Listener.Accept()
}

func TestOwnedRealityListenerTemporaryAcceptRecovery(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			tcp, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer tcp.Close()
			temporary := temporaryAcceptError()
			if wrapped {
				temporary = fmt.Errorf("listener wrapper: %w", temporary)
			}
			listener := newOwnedListener(context.Background(), &retryOnceTCPListener{Listener: tcp, err: temporary}, func(_ context.Context, raw net.Conn) (net.Conn, error) { return raw, nil })
			defer listener.Close()
			client, err := net.DialTimeout("tcp4", tcp.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			stop := time.AfterFunc(3*time.Second, func() { listener.Close() })
			defer stop.Stop()
			server, err := listener.Accept()
			if err != nil {
				t.Fatalf("one temporary Accept error prevented a valid loopback connection: %v", err)
			}
			defer server.Close()
			client.SetDeadline(time.Now().Add(time.Second))
			server.SetDeadline(time.Now().Add(time.Second))
			listener.Close()
			assertRetryTransferredConnection(t, server, client)
		})
	}
}
