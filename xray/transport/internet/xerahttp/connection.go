package xerahttp

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

type transportConn struct {
	writer                  io.WriteCloser
	reader                  io.ReadCloser
	remoteAddr              net.Addr
	localAddr               net.Addr
	onClose                 func()
	closeOnce               sync.Once
	closeErr                error
	cancel                  context.CancelFunc
	extra                   io.Closer
	deadlineMu              sync.Mutex
	readTimer               *time.Timer
	writeTimer              *time.Timer
	closed                  atomic.Bool
	timedOut                atomic.Bool
	readDeadlineGeneration  uint64
	writeDeadlineGeneration uint64
}

func (c *transportConn) Write(b []byte) (int, error) {
	if c.timedOut.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.writer.Write(b)
	if err != nil && c.timedOut.Load() {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *transportConn) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if _, ok := c.writer.(uploadWriter); ok {
		writer := &buf.BufferToBytesWriter{Writer: c}
		return writer.WriteMultiBuffer(mb)
	}
	defer func() { buf.ReleaseMulti(mb) }()
	if len(mb) == 1 {
		return buf.WriteAllBytes(c, mb[0].Bytes(), nil)
	}
	if mb.IsEmpty() {
		return nil
	}
	buffer := buf.NewWithSize(min(mb.Len(), 65536))
	defer buffer.Release()
	for !mb.IsEmpty() {
		buffer.Clear()
		data := buffer.Extend(min(mb.Len(), 65536))
		mb, _ = buf.SplitBytes(mb, data)
		if err := buf.WriteAllBytes(c, data, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *transportConn) Read(b []byte) (int, error) {
	if c.timedOut.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.reader.Read(b)
	if err != nil && c.timedOut.Load() {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *transportConn) Close() error {
	c.closed.Store(true)
	c.closeOnce.Do(func() {
		c.deadlineMu.Lock()
		if c.readTimer != nil {
			c.readTimer.Stop()
		}
		if c.writeTimer != nil {
			c.writeTimer.Stop()
		}
		c.deadlineMu.Unlock()
		if c.cancel != nil {
			c.cancel()
		}
		if c.extra != nil {
			c.extra.Close()
		}
		if c.writer != nil {
			c.closeErr = c.writer.Close()
		}
		if c.reader != nil {
			if err := c.reader.Close(); c.closeErr == nil {
				c.closeErr = err
			}
		}
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.closeErr
}

func (c *transportConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *transportConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *transportConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *transportConn) SetReadDeadline(t time.Time) error {
	return c.setDeadline(t, true)
}

func (c *transportConn) SetWriteDeadline(t time.Time) error {
	return c.setDeadline(t, false)
}

func (c *transportConn) setDeadline(t time.Time, read bool) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	timer, generation := &c.writeTimer, &c.writeDeadlineGeneration
	if read {
		timer, generation = &c.readTimer, &c.readDeadlineGeneration
	}
	*generation++
	if *timer != nil {
		(*timer).Stop()
		*timer = nil
	}
	if !t.IsZero() {
		current := *generation
		*timer = time.AfterFunc(time.Until(t), func() { c.expireDeadline(read, current) })
	}
	return nil
}

func (c *transportConn) expireDeadline(read bool, generation uint64) {
	c.deadlineMu.Lock()
	current := c.writeDeadlineGeneration
	if read {
		current = c.readDeadlineGeneration
	}
	if c.closed.Load() || current != generation {
		c.deadlineMu.Unlock()
		return
	}
	c.timedOut.Store(true)
	c.closed.Store(true)
	c.deadlineMu.Unlock()
	c.Close()
}
