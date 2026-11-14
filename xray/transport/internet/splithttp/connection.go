package splithttp

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

type splitConn struct {
	writer     io.WriteCloser
	reader     io.ReadCloser
	remoteAddr net.Addr
	localAddr  net.Addr
	onClose    func()
	closeOnce  sync.Once
	closeErr   error
	cancel     context.CancelFunc
	extra      io.Closer
}

func (c *splitConn) Write(b []byte) (int, error) {
	return c.writer.Write(b)
}

func (c *splitConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

func (c *splitConn) Close() error {
	c.closeOnce.Do(func() {
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

func (c *splitConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *splitConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *splitConn) SetDeadline(t time.Time) error {
	// TODO cannot do anything useful
	return nil
}

func (c *splitConn) SetReadDeadline(t time.Time) error {
	// TODO cannot do anything useful
	return nil
}

func (c *splitConn) SetWriteDeadline(t time.Time) error {
	// TODO cannot do anything useful
	return nil
}
