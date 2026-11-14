package xdrive

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/net"
)

var placeholderAddr = &net.TCPAddr{IP: net.IP{127, 0, 0, 1}, Port: 0}

const closeBudget = 30 * time.Second

// Conn is a session in segments, walWriter and walReader, or in slots,
// slotWriter and slotReader
type Conn struct {
	ctx           context.Context
	cancel        context.CancelFunc
	writer        io.WriteCloser
	reader        chunkReader
	onClose       func()
	waitWorkers   func()
	workers       sync.WaitGroup
	backgroundErr error
	closeTimeout  time.Duration

	readBuf []byte

	deadlineMu    sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time

	closeOnce sync.Once
	closeErr  error
}

type chunkReader interface {
	// chunks is closed when there are no more, then Err says why
	chunks() <-chan []byte
	Err() error
	Wake()
}

func newConn(ctx context.Context, storage Storage, writePrefix, readPrefix string, p params, onClose func()) *Conn {
	ctx, cancel := context.WithCancel(ctx)
	writer := newWALWriter(ctx, storage, writePrefix, p)
	reader := newWALReader(ctx, storage, readPrefix, p)
	return &Conn{
		ctx:          ctx,
		cancel:       cancel,
		writer:       writer,
		reader:       reader,
		onClose:      onClose,
		waitWorkers:  func() { writer.wait(); reader.wait() },
		closeTimeout: closeBudget,
	}
}

func newSlotConn(ctx context.Context, store slotStore, write, read slotTable, p params, first []byte, onClose func()) *Conn {
	ctx, cancel := context.WithCancel(ctx)
	writer := newSlotWriter(ctx, store, write, p)
	reader := newSlotReader(ctx, store, read, p, first)
	return &Conn{
		ctx:          ctx,
		cancel:       cancel,
		writer:       writer,
		reader:       reader,
		onClose:      onClose,
		waitWorkers:  func() { writer.wait(); reader.wait() },
		closeTimeout: closeBudget,
	}
}

func (c *Conn) Read(b []byte) (int, error) {
	if len(c.readBuf) == 0 {
		data, err := c.receive()
		if err != nil {
			return 0, err
		}
		c.readBuf = data
	}
	n := copy(b, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *Conn) receive() ([]byte, error) {
	deadline := c.getDeadline(true)
	if deadline.IsZero() {
		data, ok := <-c.reader.chunks()
		if !ok {
			return nil, c.reader.Err()
		}
		return data, nil
	}

	if !time.Now().Before(deadline) {
		return nil, os.ErrDeadlineExceeded
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case data, ok := <-c.reader.chunks():
		if !ok {
			return nil, c.reader.Err()
		}
		return data, nil
	case <-timer.C:
		return nil, os.ErrDeadlineExceeded
	}
}

func (c *Conn) Write(b []byte) (int, error) {
	if deadline := c.getDeadline(false); !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.writer.Write(b)
	if err == nil {
		c.reader.Wake()
	}
	return n, err
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		budget := c.closeTimeout
		if budget <= 0 {
			budget = closeBudget
		}
		if deadline := c.getDeadline(false); !deadline.IsZero() {
			budget = min(budget, time.Until(deadline))
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		stop := context.AfterFunc(ctx, c.cancel)
		c.closeErr = c.writer.Close()
		c.workers.Wait()
		stop()
		if ctx.Err() != nil {
			c.closeErr = ctx.Err()
		} else if c.closeErr == nil {
			c.closeErr = c.backgroundErr
		}
		cancel()
		c.cancel()
		if c.waitWorkers != nil {
			c.waitWorkers()
		}
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.closeErr
}

func (c *Conn) LocalAddr() net.Addr {
	return placeholderAddr
}

func (c *Conn) RemoteAddr() net.Addr {
	return placeholderAddr
}

func (c *Conn) getDeadline(read bool) time.Time {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if read {
		return c.readDeadline
	}
	return c.writeDeadline
}

func (c *Conn) SetDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDeadline = t
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.writeDeadline = t
	return nil
}
