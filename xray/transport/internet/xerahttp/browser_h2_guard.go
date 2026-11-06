package xerahttp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

const browserH2MaxFrameBytes = 16384
const browserH2MaxHeaderBytes = 512 << 10
const browserH2MaxHeaderFrames = 32
const browserH2HeaderTimeout = 10 * time.Second

type browserH2Guard struct {
	net.Conn
	reader        *bufio.Reader
	header        [9]byte
	headerOffset  int
	remaining     int
	headerStream  uint32
	headerBytes   int
	headerFrames  int
	endHeaders    bool
	headerTimeout time.Duration
	err           error
}

func newBrowserH2Guard(conn net.Conn) *browserH2Guard {
	return &browserH2Guard{Conn: conn, reader: bufio.NewReaderSize(conn, browserH2MaxFrameBytes), headerOffset: 9, headerTimeout: browserH2HeaderTimeout}
}

func (c *browserH2Guard) fail(err error) (int, error) {
	c.err = err
	c.Conn.Close()
	return 0, err
}

func (c *browserH2Guard) readHeader() error {
	if _, err := io.ReadFull(c.reader, c.header[:]); err != nil {
		return err
	}
	size := int(c.header[0])<<16 | int(c.header[1])<<8 | int(c.header[2])
	if size > browserH2MaxFrameBytes {
		return errors.New("HTTP/2 peer exceeded the advertised frame size")
	}
	kind, flags := c.header[3], c.header[4]
	stream := binary.BigEndian.Uint32(c.header[5:]) & 0x7fffffff
	if c.headerStream != 0 {
		if kind != 9 || stream != c.headerStream {
			return errors.New("HTTP/2 peer interleaved a header block")
		}
	} else if kind == 1 || kind == 5 {
		if stream == 0 {
			return errors.New("HTTP/2 peer sent headers on stream zero")
		}
		c.headerStream = stream
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.headerTimeout)); err != nil {
			return err
		}
	} else if kind == 9 {
		return errors.New("HTTP/2 peer sent an unexpected continuation")
	}
	if c.headerStream != 0 {
		c.headerFrames++
		c.headerBytes += size
		if c.headerFrames > browserH2MaxHeaderFrames || c.headerBytes > browserH2MaxHeaderBytes {
			return errors.New("HTTP/2 peer exceeded the header block budget")
		}
		c.endHeaders = flags&4 != 0
	}
	c.remaining = size
	c.headerOffset = 0
	return nil
}

func (c *browserH2Guard) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if c.err != nil {
		return 0, c.err
	}
	if c.headerOffset == 9 && c.remaining == 0 {
		if c.endHeaders {
			if err := c.Conn.SetReadDeadline(time.Time{}); err != nil {
				return c.fail(err)
			}
			c.headerStream, c.headerBytes, c.headerFrames = 0, 0, 0
			c.endHeaders = false
		}
		if err := c.readHeader(); err != nil {
			return c.fail(err)
		}
	}
	if c.headerOffset < 9 {
		n := copy(data, c.header[c.headerOffset:])
		c.headerOffset += n
		return n, nil
	}
	n, err := c.reader.Read(data[:min(len(data), c.remaining)])
	c.remaining -= n
	if err != nil {
		if err == io.EOF && c.remaining != 0 {
			err = io.ErrUnexpectedEOF
		}
		c.err = err
		c.Conn.Close()
	}
	return n, err
}
