package xerahttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apernet/quic-go"
	utls "github.com/refraction-networking/utls"
)

const (
	adaptiveDown           = 1
	adaptiveUp             = 2
	adaptiveWindowDuration = time.Second
	maxAdaptiveWaiters     = 256
)

var errAdaptiveQueueFull = errors.New("Xera adaptive HTTP request queue capacity exhausted")

type adaptiveWindow struct {
	bytes     int64
	failures  int
	transient int
	latency   time.Duration
	rtt       time.Duration
}

type adaptiveDirection struct {
	limit         int
	active        int
	healthy       int
	window        adaptiveWindow
	baseline      time.Duration
	baselineAt    time.Time
	latency       time.Duration
	rtt           time.Duration
	rttBaseline   time.Duration
	rttBaselineAt time.Time
	cooldown      time.Time
	probeRate     float64
	probing       bool
	blockedRate   float64
}

type adaptiveWaiter struct {
	mask  int
	since time.Time
}

type adaptiveController struct {
	mu         sync.Mutex
	mode       string
	minimum    int
	maximum    int
	directions [2]adaptiveDirection
	waiters    []*adaptiveWaiter
	wake       chan struct{}
	closed     bool
	now        func() time.Time
	windowAt   time.Time
	startedAt  time.Time
}

func newAdaptiveController(config *AdaptiveConfig) *adaptiveController {
	if config == nil {
		return nil
	}
	c := &adaptiveController{mode: config.Mode, minimum: int(config.MinStreams), maximum: int(config.MaxStreams), wake: make(chan struct{}), now: time.Now, windowAt: time.Now()}
	c.startedAt = c.windowAt
	for i := range c.directions {
		c.directions[i].limit = min(c.maximum, max(c.minimum, initialAdaptiveStreams))
	}
	return c
}

func (c *adaptiveController) signalLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}

func (c *adaptiveController) availableLocked(mask int) bool {
	for i := range c.directions {
		if mask&(1<<i) != 0 && c.directions[i].active >= c.directions[i].limit {
			return false
		}
	}
	return true
}

func (c *adaptiveController) removeWaiterLocked(waiter *adaptiveWaiter) {
	for i, item := range c.waiters {
		if item == waiter {
			copy(c.waiters[i:], c.waiters[i+1:])
			c.waiters[len(c.waiters)-1] = nil
			c.waiters = c.waiters[:len(c.waiters)-1]
			c.signalLocked()
			return
		}
	}
}

func (c *adaptiveController) acquire(ctx context.Context, mask int) (*adaptiveLease, error) {
	if c == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	now := c.now()
	c.stepLocked(now)
	canPass := c.availableLocked(mask)
	for _, waiter := range c.waiters {
		if waiter.mask&mask != 0 && c.availableLocked(waiter.mask) {
			canPass = false
			break
		}
	}
	if canPass {
		lease := c.reserveLocked(mask, now)
		c.mu.Unlock()
		return lease, nil
	}
	if len(c.waiters) >= maxAdaptiveWaiters {
		c.mu.Unlock()
		return nil, errAdaptiveQueueFull
	}
	waiter := &adaptiveWaiter{mask: mask, since: c.now()}
	c.waiters = append(c.waiters, waiter)
	for {
		now := c.now()
		c.stepLocked(now)
		if err := ctx.Err(); err != nil {
			c.removeWaiterLocked(waiter)
			c.mu.Unlock()
			return nil, err
		}
		if c.closed {
			c.removeWaiterLocked(waiter)
			c.mu.Unlock()
			return nil, net.ErrClosed
		}
		var next *adaptiveWaiter
		for _, candidate := range c.waiters {
			if c.availableLocked(candidate.mask) {
				next = candidate
				break
			}
		}
		if next == waiter {
			c.removeWaiterLocked(waiter)
			lease := c.reserveLocked(mask, now)
			c.mu.Unlock()
			return lease, nil
		}
		wake := c.wake
		delay := max(time.Millisecond, adaptiveWindowDuration-now.Sub(c.windowAt))
		c.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
		c.mu.Lock()
	}
}

func (c *adaptiveController) reserveLocked(mask int, now time.Time) *adaptiveLease {
	for i := range c.directions {
		if mask&(1<<i) != 0 {
			c.directions[i].active++
		}
	}
	return &adaptiveLease{controller: c, mask: mask, started: now}
}

func (c *adaptiveController) stepLocked(now time.Time) {
	elapsed := now.Sub(c.windowAt)
	if elapsed < adaptiveWindowDuration {
		return
	}
	stable, inflation, reduction, cooldown := 3, 1.3, 0.75, 3*time.Second
	queueTarget := 20 * time.Millisecond
	switch c.mode {
	case AdaptiveLowLatency:
		stable, inflation = 2, 1.15
		queueTarget = 5 * time.Millisecond
	case AdaptiveUnstableNetwork:
		stable, reduction, cooldown = 5, 0.5, 10*time.Second
		queueTarget = 50 * time.Millisecond
	}
	for i := range c.directions {
		d := &c.directions[i]
		demand := false
		for _, waiter := range c.waiters {
			if waiter.mask&(1<<i) != 0 && now.Sub(waiter.since) >= queueTarget {
				demand = true
				break
			}
		}
		rate := float64(d.window.bytes) / elapsed.Seconds()
		congested := d.window.transient > 0 ||
			d.window.latency > 0 && d.baseline > 0 && d.latency > d.baseline+queueTarget && float64(d.latency) > float64(d.baseline)*inflation ||
			d.window.rtt > 0 && d.rttBaseline > 0 && d.rtt > d.rttBaseline+queueTarget && float64(d.rtt) > float64(d.rttBaseline)*inflation
		switch {
		case congested:
			d.limit = max(c.minimum, int(math.Ceil(float64(d.limit)*reduction)))
			d.healthy, d.probing = 0, false
			d.blockedRate = 0
			d.cooldown = now.Add(cooldown)
		case d.window.failures > 0 || !demand || rate == 0 || elapsed > 5*adaptiveWindowDuration:
			d.healthy = 0
			if !demand {
				d.probing = false
				d.blockedRate = 0
			}
		case now.Before(d.cooldown):
			d.healthy = 0
		case d.probing:
			d.probing = false
			d.healthy = 0
			gain := 1 + min(0.05, 0.5/float64(max(1, d.limit-1)))
			if rate <= d.probeRate*gain {
				d.limit = max(c.minimum, d.limit-1)
				d.blockedRate = max(rate, d.probeRate)
				d.cooldown = now.Add(cooldown)
			}
		case d.blockedRate > 0 && rate <= d.blockedRate*1.1:
			d.healthy = 0
		default:
			d.blockedRate = 0
			if d.limit >= c.maximum {
				d.healthy = 0
				break
			}
			d.healthy++
			if d.healthy >= stable && d.limit < c.maximum {
				d.limit++
				d.probing, d.probeRate, d.healthy = true, rate, 0
			}
		}
		d.window = adaptiveWindow{}
	}
	c.windowAt = now
	c.signalLocked()
}

func (c *adaptiveController) retire() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	c.signalLocked()
	c.mu.Unlock()
}

func (c *adaptiveController) busy() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters) > 0 || c.directions[0].active > 0 || c.directions[1].active > 0
}

type adaptiveLease struct {
	controller         *adaptiveController
	mask               int
	started            time.Time
	once               sync.Once
	confirmed          atomic.Bool
	rejected           atomic.Bool
	finished           atomic.Bool
	buffered           [2]atomic.Int64
	passiveRTT         func() time.Duration
	nextRTTSample      atomic.Int64
	requestBodyPending atomic.Bool
	responseEnded      atomic.Bool
}

func (l *adaptiveLease) confirm() {
	l.sampleRTT()
	l.confirmProgress(true)
}

func (l *adaptiveLease) sampleRTT() {
	if l == nil || l.passiveRTT == nil || l.finished.Load() {
		return
	}
	c := l.controller
	now := c.now()
	next := l.nextRTTSample.Load()
	elapsed := now.Sub(c.startedAt).Nanoseconds()
	if elapsed < next || !l.nextRTTSample.CompareAndSwap(next, elapsed+int64(time.Second)) {
		return
	}
	rtt := l.passiveRTT()
	if rtt <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.finished.Load() {
		return
	}
	c.stepLocked(now)
	for i := range c.directions {
		if l.mask&(1<<i) == 0 {
			continue
		}
		d := &c.directions[i]
		if d.rttBaseline == 0 || rtt < d.rttBaseline || now.Sub(d.rttBaselineAt) >= time.Minute {
			d.rttBaseline, d.rttBaselineAt = rtt, now
		}
		if d.rtt == 0 {
			d.rtt = rtt
		} else {
			d.rtt = (d.rtt*4 + rtt) / 5
		}
		d.window.rtt = rtt
	}
}

func (l *adaptiveLease) confirmProgress(measureLatency bool) {
	if l == nil || !l.confirmed.CompareAndSwap(false, true) || l.finished.Load() {
		return
	}
	c := l.controller
	c.mu.Lock()
	if l.finished.Load() {
		c.mu.Unlock()
		return
	}
	now := c.now()
	c.stepLocked(now)
	latency := now.Sub(l.started)
	for i := range c.directions {
		if l.mask&(1<<i) == 0 {
			continue
		}
		d := &c.directions[i]
		d.window.bytes += l.buffered[i].Swap(0)
		if !measureLatency {
			continue
		}
		if d.baseline == 0 || latency < d.baseline || now.Sub(d.baselineAt) >= time.Minute {
			d.baseline, d.baselineAt = latency, now
		}
		if d.latency == 0 {
			d.latency = latency
		} else {
			d.latency = (d.latency*4 + latency) / 5
		}
		d.window.latency = latency
	}
	c.mu.Unlock()
}

func (l *adaptiveLease) progress(direction, n int) {
	if l == nil || n <= 0 || l.finished.Load() || l.mask&direction == 0 {
		return
	}
	l.sampleRTT()
	i := 0
	if direction == adaptiveUp {
		i = 1
	}
	l.buffered[i].Add(int64(n))
	if !l.confirmed.Load() {
		return
	}
	c := l.controller
	c.mu.Lock()
	if l.finished.Load() {
		c.mu.Unlock()
		return
	}
	c.stepLocked(c.now())
	c.directions[i].window.bytes += l.buffered[i].Swap(0)
	c.mu.Unlock()
}

func (l *adaptiveLease) reject(status int) {
	if l == nil || !l.rejected.CompareAndSwap(false, true) {
		return
	}
	c := l.controller
	c.mu.Lock()
	c.stepLocked(c.now())
	for i := range c.directions {
		if l.mask&(1<<i) != 0 {
			c.directions[i].window.failures++
			if status == 429 || status == 503 {
				c.directions[i].window.transient++
			}
		}
	}
	c.mu.Unlock()
}

func (l *adaptiveLease) finish(err error) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.finished.Store(true)
		c := l.controller
		c.mu.Lock()
		c.stepLocked(c.now())
		for i := range c.directions {
			if l.mask&(1<<i) == 0 {
				continue
			}
			d := &c.directions[i]
			d.active--
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !l.rejected.Load() {
				d.window.failures++
				if isAdaptiveCongestionError(err) {
					d.window.transient++
				}
			}
		}
		c.signalLocked()
		c.mu.Unlock()
	})
}

func isAdaptiveCongestionError(err error) bool {
	var certificate *tls.CertificateVerificationError
	var uCertificate *utls.CertificateVerificationError
	var roots x509.SystemRootsError
	if errors.As(err, &certificate) || errors.As(err, &uCertificate) || errors.As(err, &roots) {
		return false
	}
	var transport *quic.TransportError
	if errors.As(err, &transport) && transport != nil && (transport.ErrorCode.IsCryptoError() || transport.ErrorCode == quic.ProtocolViolation) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	if runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(10053)) || errors.Is(err, syscall.Errno(10054)) || errors.Is(err, syscall.Errno(10060))) {
		return true
	}
	var operation *net.OpError
	if errors.As(err, &operation) && operation != nil && operation.Timeout() {
		return true
	}
	var network net.Error
	if errors.As(err, &network) && network != nil && network.Timeout() {
		return true
	}
	var idle *quic.IdleTimeoutError
	var handshake *quic.HandshakeTimeoutError
	var reset *quic.StatelessResetError
	return errors.As(err, &idle) || errors.As(err, &handshake) || errors.As(err, &reset)
}

func (l *adaptiveLease) endRequestBody() {
	if l == nil {
		return
	}
	l.requestBodyPending.Store(false)
	if l.responseEnded.Load() {
		l.finish(nil)
	}
}

func (l *adaptiveLease) endResponse(err error) {
	if l == nil {
		return
	}
	if err != nil && !errors.Is(err, io.EOF) {
		l.finish(err)
		return
	}
	l.responseEnded.Store(true)
	if !l.requestBodyPending.Load() {
		l.finish(nil)
	}
}

type adaptiveReader struct {
	io.ReadCloser
	lease             *adaptiveLease
	direction         int
	release           bool
	confirmOnProgress bool
	ctx               context.Context
	requestBody       bool
	onClose           func()
	closeOnce         sync.Once
	closeErr          error
}

func (r *adaptiveReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.confirmOnProgress {
		r.lease.confirmProgress(false)
	}
	r.lease.progress(r.direction, n)
	if err != nil && r.requestBody {
		r.lease.endRequestBody()
	}
	if err != nil && r.release {
		failure := err
		if r.ctx != nil && r.ctx.Err() != nil {
			failure = nil
		}
		r.lease.endResponse(failure)
	}
	return n, err
}

func (r *adaptiveReader) Close() error {
	r.closeOnce.Do(func() {
		if r.onClose != nil {
			r.onClose()
		}
		r.closeErr = r.ReadCloser.Close()
		if r.requestBody {
			r.lease.endRequestBody()
		}
		if r.release {
			r.lease.endResponse(nil)
		}
	})
	return r.closeErr
}
