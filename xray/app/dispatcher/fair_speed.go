//go:build !wasm

package dispatcher

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"golang.org/x/time/rate"
)

const fairSpeedQuantum = 16 * 1024
const fairSpeedMaxPending = 65536
const fairSpeedMaxOwnerPending = 8192

type fairSpeedRequest struct {
	ctx       context.Context
	remaining int
	done      chan error
	owner     *fairSpeedOwner
	device    *fairSpeedDevice
	element   *list.Element
}

type fairSpeedDevice struct {
	key    string
	served uint64
	queue  list.List
}

type fairSpeedOwner struct {
	key         string
	served      uint64
	deviceClock uint64
	pending     int
	devices     map[string]*fairSpeedDevice
}

// The shared limiter enforces the ceiling. Virtual byte counters choose the
// next owner, then its next authenticated device, independently of flow count.
// Only backlogged identities participate; idle identities reserve no capacity.
type fairSpeedLimiter struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	owners  map[string]*fairSpeedOwner
	clock   uint64
	pending int
	running bool
}

func newFairSpeedLimiter(limiter *rate.Limiter) *fairSpeedLimiter {
	return &fairSpeedLimiter{limiter: limiter, owners: make(map[string]*fairSpeedOwner)}
}

func (f *fairSpeedLimiter) idle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending == 0 && !f.running
}

func (f *fairSpeedLimiter) wait(ctx context.Context, ownerKey, deviceKey string, size int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size <= 0 {
		return nil
	}
	f.mu.Lock()
	owner := f.owners[ownerKey]
	if f.pending >= fairSpeedMaxPending || (owner != nil && owner.pending >= fairSpeedMaxOwnerPending) {
		f.mu.Unlock()
		return errors.New("speed admission queue is full")
	}
	if owner == nil {
		owner = &fairSpeedOwner{key: ownerKey, served: f.clock, devices: make(map[string]*fairSpeedDevice)}
		f.owners[ownerKey] = owner
	}
	device := owner.devices[deviceKey]
	if device == nil {
		device = &fairSpeedDevice{key: deviceKey, served: owner.deviceClock}
		owner.devices[deviceKey] = device
	}
	req := &fairSpeedRequest{ctx: ctx, remaining: size, done: make(chan error, 1), owner: owner, device: device}
	req.element = device.queue.PushBack(req)
	owner.pending++
	f.pending++
	if !f.running {
		f.running = true
		go f.run()
	}
	f.mu.Unlock()
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		f.mu.Lock()
		f.finishLocked(req, ctx.Err())
		f.mu.Unlock()
		return ctx.Err()
	}
}

func (f *fairSpeedLimiter) finishLocked(req *fairSpeedRequest, err error) {
	if req.element == nil {
		return
	}
	req.device.queue.Remove(req.element)
	req.element = nil
	req.owner.pending--
	f.pending--
	if req.device.queue.Len() == 0 {
		delete(req.owner.devices, req.device.key)
	}
	if req.owner.pending == 0 {
		delete(f.owners, req.owner.key)
	}
	req.done <- err
}

func (f *fairSpeedLimiter) nextLocked() *fairSpeedRequest {
	var owner *fairSpeedOwner
	for _, candidate := range f.owners {
		if owner == nil || candidate.served < owner.served || (candidate.served == owner.served && candidate.key < owner.key) {
			owner = candidate
		}
	}
	if owner == nil {
		return nil
	}
	var device *fairSpeedDevice
	for _, candidate := range owner.devices {
		if device == nil || candidate.served < device.served || (candidate.served == device.served && candidate.key < device.key) {
			device = candidate
		}
	}
	return device.queue.Front().Value.(*fairSpeedRequest)
}

func (f *fairSpeedLimiter) advanceLocked(req *fairSpeedRequest, size int) {
	req.owner.served += uint64(size)
	req.device.served += uint64(size)
	clock := req.owner.served
	for _, owner := range f.owners {
		clock = min(clock, owner.served)
	}
	f.clock = max(f.clock, clock)
	deviceClock := req.device.served
	for _, device := range req.owner.devices {
		deviceClock = min(deviceClock, device.served)
	}
	req.owner.deviceClock = max(req.owner.deviceClock, deviceClock)
}

func (f *fairSpeedLimiter) run() {
	for {
		f.mu.Lock()
		req := f.nextLocked()
		if req == nil {
			f.running = false
			f.mu.Unlock()
			return
		}
		size := min(req.remaining, fairSpeedQuantum)
		f.mu.Unlock()
		err := f.limiter.WaitN(req.ctx, size)
		f.mu.Lock()
		if req.element != nil {
			if err != nil {
				f.finishLocked(req, err)
			} else {
				f.advanceLocked(req, size)
				req.remaining -= size
				if req.remaining == 0 {
					f.finishLocked(req, nil)
				}
			}
		}
		f.mu.Unlock()
	}
}
