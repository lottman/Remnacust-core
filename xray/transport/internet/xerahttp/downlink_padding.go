package xerahttp

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"io"
	"strings"
	"sync"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/net/http/httpguts"
)

const maxDownlinkBlock = 16384
const maxDownlinkPadding = 1024

func (p *DownlinkPaddingConfig) paddingRange() *RangeConfig {
	if p.Bytes == nil {
		return &RangeConfig{From: 16, To: 128}
	}
	return p.Bytes
}

func (p *DownlinkPaddingConfig) blockRange() *RangeConfig {
	if p.BlockBytes == nil {
		return &RangeConfig{From: 4096, To: 8192}
	}
	return p.BlockBytes
}

func (p *DownlinkPaddingConfig) wireValue() string {
	if p.Uplink {
		return "2." + p.Token
	}
	return "1." + p.Token
}

func (p *DownlinkPaddingConfig) Validate() error {
	if p.BudgetPercent > 100 || p.BurstBytes > 1048576 || (p.BudgetPercent == 0 && p.BurstBytes != 0) {
		return errors.New("padding budgetPercent must be 1..100 when burstBytes is configured, burstBytes must not exceed 1048576")
	}
	switch strings.ToLower(p.Header) {
	case "x-accel-buffering", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
		return errors.New("customDownlinkPadding.header conflicts with proxy headers")
	}
	if len(p.Header) < 3 || len(p.Header) > 64 || !strings.HasPrefix(strings.ToLower(p.Header), "x-") || !httpguts.ValidHeaderFieldName(p.Header) {
		return errors.New("customDownlinkPadding.header must be an X- header of 3..64 characters")
	}
	if len(p.Token) < 16 || len(p.Token) > 128 || strings.Trim(p.Token, charsetBase62+"-_") != "" {
		return errors.New("customDownlinkPadding.token must contain 16..128 base64url characters")
	}
	padding, block := p.paddingRange(), p.blockRange()
	if padding.From < 0 || padding.To < padding.From || padding.To > maxDownlinkPadding {
		return errors.New("customDownlinkPadding.bytes must be within 0..1024")
	}
	if block.From < 1024 || block.To < block.From || block.To > maxDownlinkBlock {
		return errors.New("customDownlinkPadding.blockBytes must be within 1024..16384")
	}
	return nil
}

type paddedDownlinkWriter struct {
	io.WriteCloser
	config *DownlinkPaddingConfig
	mu     sync.Mutex
	err    error
	budget paddingAllowance
}

func (w *paddedDownlinkWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(data) > 0 {
		n := min(len(data), int(w.config.blockRange().rand()))
		padding := w.budget.next(w.config, n)
		frame := buf.NewWithSize(int32(4 + n + padding))
		wire := frame.Extend(int32(4 + n + padding))
		binary.BigEndian.PutUint16(wire[:2], uint16(n))
		binary.BigEndian.PutUint16(wire[2:4], uint16(padding))
		copy(wire[4:], data[:n])
		if _, err := rand.Read(wire[4+n:]); err != nil {
			frame.Release()
			w.err = err
			return written, err
		}
		count, err := w.WriteCloser.Write(wire)
		if err == nil && count != len(wire) {
			err = io.ErrShortWrite
		}
		frame.Release()
		if err != nil {
			w.err = err
			return written + min(n, max(0, count-4)), err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}

type paddedDownlinkReader struct {
	io.ReadCloser
	mu        sync.Mutex
	buffered  *bufio.Reader
	remaining int
	padding   int
	scratch   [maxDownlinkPadding]byte
	err       error
}

func (r *paddedDownlinkReader) Read(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(data) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.buffered == nil {
		r.buffered = bufio.NewReaderSize(r.ReadCloser, maxDownlinkBlock)
	}
	if r.remaining == 0 {
		if r.padding > 0 {
			if _, err := io.ReadFull(r.buffered, r.scratch[:r.padding]); err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				r.err = err
				return 0, err
			}
			r.padding = 0
		}
		header := r.scratch[:4]
		if _, err := io.ReadFull(r.buffered, header); err != nil {
			r.err = err
			return 0, err
		}
		r.remaining = int(binary.BigEndian.Uint16(header[:2]))
		r.padding = int(binary.BigEndian.Uint16(header[2:]))
		if r.remaining == 0 || r.remaining > maxDownlinkBlock || r.padding > maxDownlinkPadding {
			r.err = errors.New("invalid custom downlink frame length")
			return 0, r.err
		}
	}
	n, err := r.buffered.Read(data[:min(len(data), r.remaining)])
	r.remaining -= n
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	r.err = err
	return n, err
}
