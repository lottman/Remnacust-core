package xerahttp

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"

	"github.com/xtls/xray-core/common/buf"
)

type paddingAllowance struct {
	initialized bool
	credit      int64
}

type limitedUploadBody struct {
	io.Reader
	io.Closer
}

type uploadPaddingKey struct{}

type sharedPaddingAllowance struct {
	mu     sync.Mutex
	budget paddingAllowance
}

func (a *sharedPaddingAllowance) next(config *DownlinkPaddingConfig, size int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.budget.next(config, size)
}

func (a *paddingAllowance) next(config *DownlinkPaddingConfig, size int) int {
	padding := int(config.paddingRange().rand())
	if config.BudgetPercent == 0 {
		return padding
	}
	burst := int64(config.BurstBytes)
	if burst == 0 {
		burst = 4096
	}
	if !a.initialized {
		a.initialized = true
		a.credit = burst * 100
	}
	a.credit = min(burst*100, a.credit+int64(size)*int64(config.BudgetPercent))
	padding = min(padding, int(a.credit/100))
	a.credit -= int64(padding) * 100
	return padding
}

type paddedUplinkReader struct {
	io.ReadCloser
	config    *DownlinkPaddingConfig
	mu        sync.Mutex
	closed    atomic.Bool
	frame     *buf.Buffer
	budget    paddingAllowance
	allowance *sharedPaddingAllowance
	err       error
	plan      []paddingFrame
}

type paddingFrame struct {
	size    uint16
	padding uint16
}

func planPaddedPacket(source io.ReadCloser, config *DownlinkPaddingConfig, allowance *sharedPaddingAllowance, length int64) (*paddedUplinkReader, int64) {
	r := &paddedUplinkReader{ReadCloser: source, config: config, plan: make([]paddingFrame, 0)}
	var wireLength int64
	for length > 0 {
		size := int(min(length, int64(config.blockRange().rand())))
		var padding int
		if allowance != nil {
			padding = allowance.next(config, size)
		} else {
			padding = r.budget.next(config, size)
		}
		r.plan = append(r.plan, paddingFrame{size: uint16(size), padding: uint16(padding)})
		wireLength += int64(4 + size + padding)
		length -= int64(size)
	}
	return r, wireLength
}

func (r *paddedUplinkReader) Read(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if len(data) == 0 {
		return 0, nil
	}
	if r.frame == nil {
		if r.err != nil {
			return 0, r.err
		}
		var size, padding int
		if r.plan != nil {
			if len(r.plan) == 0 {
				return 0, io.EOF
			}
			size, padding = int(r.plan[0].size), int(r.plan[0].padding)
		} else {
			size = int(r.config.blockRange().rand())
		}
		frame := buf.NewWithSize(int32(4 + size + maxDownlinkPadding))
		wire := frame.Extend(int32(4 + size + maxDownlinkPadding))
		var n int
		var err error
		if r.plan != nil {
			n, err = io.ReadFull(r.ReadCloser, wire[4:4+size])
			if err != nil {
				frame.Release()
				r.err = io.ErrUnexpectedEOF
				return 0, r.err
			}
			r.plan = r.plan[1:]
		} else {
			n, err = r.ReadCloser.Read(wire[4 : 4+size])
		}
		r.err = err
		if n == 0 {
			frame.Release()
			return 0, err
		}
		if r.plan == nil {
			if r.allowance != nil {
				padding = r.allowance.next(r.config, n)
			} else {
				padding = r.budget.next(r.config, n)
			}
		}
		binary.BigEndian.PutUint16(wire[:2], uint16(n))
		binary.BigEndian.PutUint16(wire[2:4], uint16(padding))
		if _, err := rand.Read(wire[4+n : 4+n+padding]); err != nil {
			frame.Release()
			r.err = err
			return 0, err
		}
		frame.Resize(0, int32(4+n+padding))
		r.frame = frame
	}
	n, _ := r.frame.Read(data)
	if r.frame.IsEmpty() {
		r.frame.Release()
		r.frame = nil
	}
	return n, nil
}

func (r *paddedUplinkReader) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	err := r.ReadCloser.Close()
	r.mu.Lock()
	r.plan = nil
	if r.frame != nil {
		r.frame.Release()
		r.frame = nil
	}
	r.mu.Unlock()
	return err
}
