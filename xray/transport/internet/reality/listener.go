package reality

import (
	"context"
	"net"
	"sync"

	goreality "github.com/xtls/reality"
)

type listenerResult struct{ conn, raw net.Conn }

// The result queue is never closed: a successful handshake racing shutdown may
// still publish. Closing done cancels publishers and wakes all Accept callers.
type ownedListener struct {
	net.Listener
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	closed     bool
	err        error
	pending    map[net.Conn]struct{}
	done       chan struct{}
	ready      chan listenerResult
	slots      chan struct{}
	handshakes sync.WaitGroup
	handshake  func(context.Context, net.Conn) (net.Conn, error)
}

// NewListener retains the official REALITY handshake and wire format while
// owning pending handshakes and shutdown locally instead of closing a send queue.
func NewListener(ctx context.Context, inner net.Listener, config *goreality.Config) net.Listener {
	go goreality.DetectPostHandshakeRecordsLens(config)
	return newOwnedListener(ctx, inner, func(ctx context.Context, raw net.Conn) (net.Conn, error) { return goreality.Server(ctx, raw, config) })
}

func newOwnedListener(ctx context.Context, inner net.Listener, handshake func(context.Context, net.Conn) (net.Conn, error)) *ownedListener {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &ownedListener{Listener: inner, ctx: ctx, cancel: cancel, pending: make(map[net.Conn]struct{}), done: make(chan struct{}), ready: make(chan listenerResult, 16), slots: make(chan struct{}, 256), handshake: handshake}
	go l.serve()
	return l
}

func (l *ownedListener) stop(err error) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.err = err
	close(l.done)
	l.cancel()
	pending := make([]net.Conn, 0, len(l.pending))
	for conn := range l.pending {
		pending = append(pending, conn)
	}
	l.mu.Unlock()
	closeErr := l.Listener.Close()
	for _, conn := range pending {
		_ = conn.Close()
	}
	return closeErr
}

func (l *ownedListener) Close() error { return l.stop(net.ErrClosed) }

func (l *ownedListener) release(raw net.Conn) {
	l.mu.Lock()
	if _, ok := l.pending[raw]; ok {
		delete(l.pending, raw)
		<-l.slots
	}
	l.mu.Unlock()
}

func (l *ownedListener) serve() {
	for {
		select {
		case l.slots <- struct{}{}:
		case <-l.done:
			return
		}
		raw, err := l.Listener.Accept()
		if err != nil {
			<-l.slots
			_ = l.stop(err)
			return
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = raw.Close()
			<-l.slots
			return
		}
		l.pending[raw] = struct{}{}
		l.handshakes.Add(1)
		l.mu.Unlock()
		go l.authenticate(raw)
	}
}

func (l *ownedListener) authenticate(raw net.Conn) {
	defer l.handshakes.Done()
	conn, err := l.handshake(l.ctx, raw)
	if err != nil {
		_ = raw.Close()
		l.release(raw)
		return
	}
	select {
	case l.ready <- listenerResult{conn: conn, raw: raw}:
		l.mu.Lock()
		_, pending := l.pending[raw]
		closed := l.closed
		if closed && pending {
			delete(l.pending, raw)
			<-l.slots
		}
		l.mu.Unlock()
		if closed && pending {
			_ = conn.Close()
		}
	case <-l.done:
		_ = conn.Close()
		l.release(raw)
	}
}

func (l *ownedListener) Accept() (net.Conn, error) {
	select {
	case result := <-l.ready:
		l.mu.Lock()
		closed, err := l.closed, l.err
		if !closed {
			delete(l.pending, result.raw)
			<-l.slots
		}
		l.mu.Unlock()
		if closed {
			_ = result.conn.Close()
			l.release(result.raw)
			return nil, err
		}
		return result.conn, nil
	case <-l.done:
		l.mu.Lock()
		err := l.err
		l.mu.Unlock()
		return nil, err
	}
}
