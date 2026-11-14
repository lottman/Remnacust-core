package xerahttp

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const browserH2WriteBatchBytes = 64 << 10
const browserH2WriteCloseTimeout = 250 * time.Millisecond

type browserH2WriteConn struct {
	net.Conn
	mu            sync.Mutex
	deadlineMu    sync.Mutex
	pending       []byte
	active        int
	wake          chan struct{}
	writers       chan struct{}
	done          chan struct{}
	writeDeadline time.Time
	closing       bool
	err           error
	closeErr      error
	closeOnce     sync.Once
	rawCloseOnce  sync.Once
	abortOnce     sync.Once
}

func newBrowserH2WriteConn(conn net.Conn) *browserH2WriteConn {
	c := &browserH2WriteConn{
		Conn: conn, pending: make([]byte, 0, browserH2WriteBatchBytes),
		wake: make(chan struct{}), writers: make(chan struct{}, 1), done: make(chan struct{}),
	}
	go c.run()
	return c
}

func (c *browserH2WriteConn) signalLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}

func (c *browserH2WriteConn) writeErrorLocked() error {
	if c.err != nil {
		return c.err
	}
	if c.closing {
		return net.ErrClosed
	}
	if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		return os.ErrDeadlineExceeded
	}
	return nil
}

func browserH2WaitWrite(wake <-chan struct{}, writers chan<- struct{}, deadline time.Time) bool {
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case writers <- struct{}{}:
		return true
	case <-wake:
	case <-timeout:
	}
	return false
}

func (c *browserH2WriteConn) Write(data []byte) (int, error) {
	for {
		c.mu.Lock()
		err, wake, deadline := c.writeErrorLocked(), c.wake, c.writeDeadline
		c.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if browserH2WaitWrite(wake, c.writers, deadline) {
			break
		}
	}
	defer func() { <-c.writers }()
	written := 0
	for {
		c.mu.Lock()
		if err := c.writeErrorLocked(); err != nil {
			c.mu.Unlock()
			return written, err
		}
		if written == len(data) {
			c.mu.Unlock()
			return written, nil
		}
		available := browserH2WriteBatchBytes - len(c.pending)
		if available > 0 {
			n := min(available, len(data)-written)
			c.pending = append(c.pending, data[written:written+n]...)
			written += n
			c.signalLocked()
			c.mu.Unlock()
			continue
		}
		wake, deadline := c.wake, c.writeDeadline
		c.mu.Unlock()
		browserH2WaitWrite(wake, nil, deadline)
	}
}

func (c *browserH2WriteConn) Read(data []byte) (int, error) {
	c.mu.Lock()
	failure := c.err
	c.mu.Unlock()
	if failure != nil {
		return 0, failure
	}
	n, err := c.Conn.Read(data)
	c.mu.Lock()
	failure = c.err
	c.mu.Unlock()
	if failure != nil {
		return n, failure
	}
	return n, err
}

func (c *browserH2WriteConn) closeRaw() {
	c.rawCloseOnce.Do(func() {
		err := c.Conn.Close()
		c.mu.Lock()
		c.closeErr = err
		c.mu.Unlock()
	})
}

func (c *browserH2WriteConn) abortRaw() {
	c.deadlineMu.Lock()
	c.Conn.SetDeadline(time.Now())
	c.deadlineMu.Unlock()
	c.abortOnce.Do(func() {
		if secured, ok := c.Conn.(interface{ NetConn() net.Conn }); ok {
			secured.NetConn().Close()
		}
	})
	c.closeRaw()
}

func (c *browserH2WriteConn) run() {
	defer close(c.done)
	defer c.closeRaw()
	batch := make([]byte, browserH2WriteBatchBytes)
	for {
		c.mu.Lock()
		if c.err != nil || c.closing && len(c.pending) == 0 {
			c.mu.Unlock()
			return
		}
		if len(c.pending) == 0 {
			wake := c.wake
			c.mu.Unlock()
			<-wake
			continue
		}
		n := copy(batch, c.pending)
		c.pending = c.pending[:0]
		c.active = n
		c.signalLocked()
		c.mu.Unlock()
		remaining := batch[:n]
		var failure error
		for len(remaining) > 0 {
			count, err := c.Conn.Write(remaining)
			if count < 0 || count > len(remaining) {
				failure = io.ErrShortWrite
				break
			}
			remaining = remaining[count:]
			if err != nil {
				failure = err
				break
			}
			if count == 0 {
				failure = io.ErrNoProgress
				break
			}
		}
		c.mu.Lock()
		c.active = 0
		if failure != nil && c.err == nil {
			c.err = failure
			c.pending = c.pending[:0]
		}
		c.signalLocked()
		c.mu.Unlock()
		if failure != nil {
			c.abortRaw()
			return
		}
	}
}

func (c *browserH2WriteConn) deadlineError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.closing {
		return net.ErrClosed
	}
	return nil
}

func (c *browserH2WriteConn) SetReadDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if err := c.deadlineError(); err != nil {
		return err
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *browserH2WriteConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if err := c.deadlineError(); err != nil {
		return err
	}
	if err := c.Conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	c.mu.Lock()
	c.writeDeadline = deadline
	c.signalLocked()
	c.mu.Unlock()
	return nil
}

func (c *browserH2WriteConn) SetDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if err := c.deadlineError(); err != nil {
		return err
	}
	if err := c.Conn.SetDeadline(deadline); err != nil {
		return err
	}
	c.mu.Lock()
	c.writeDeadline = deadline
	c.signalLocked()
	c.mu.Unlock()
	return nil
}

func (c *browserH2WriteConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.signalLocked()
		c.mu.Unlock()
		timer := time.NewTimer(browserH2WriteCloseTimeout)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			c.mu.Lock()
			if c.err == nil {
				c.err = net.ErrClosed
			}
			c.pending = c.pending[:0]
			c.signalLocked()
			c.mu.Unlock()
			c.abortRaw()
			<-c.done
		}
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return c.closeErr
}
