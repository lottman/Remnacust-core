package inbound

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
)

// RuntimeGeneration owns routing resources until the last pinned session ends.
// Admission never changes the dispatcher of an existing transport session.
type RuntimeGeneration struct {
	Context    context.Context
	Dispatcher routing.Dispatcher
	mu         sync.Mutex
	owners     int
	sessions   int
	closed     bool
	close      func()
}

func NewRuntimeGeneration(ctx context.Context, dispatcher routing.Dispatcher, close func()) *RuntimeGeneration {
	return &RuntimeGeneration{Context: ctx, Dispatcher: dispatcher, close: close}
}

func (g *RuntimeGeneration) addOwner() { g.mu.Lock(); g.owners++; g.mu.Unlock() }

// Retain keeps the applied generation available even with zero public ports.
func (g *RuntimeGeneration) Retain() func() {
	g.addOwner()
	var once sync.Once
	return func() {
		once.Do(func() {
			if close := g.removeOwner(); close != nil {
				close()
			}
		})
	}
}

func (g *RuntimeGeneration) removeOwner() func() {
	g.mu.Lock()
	g.owners--
	close := g.collectLocked()
	g.mu.Unlock()
	return close
}

func (g *RuntimeGeneration) collectLocked() func() {
	if g.owners == 0 && g.sessions == 0 && !g.closed {
		g.closed = true
		return g.close
	}
	return nil
}

// RuntimeSlot is shared by all workers of one native inbound. A port's native
// listener and authentication proxy survive routing-generation changes.
type RuntimeSlot struct {
	barrier    *sync.RWMutex
	mu         sync.Mutex
	generation *RuntimeGeneration
	active     int
	accepting  bool
	onDrain    func()
}

func NewRuntimeSlot(barrier *sync.RWMutex, generation *RuntimeGeneration) *RuntimeSlot {
	generation.addOwner()
	return &RuntimeSlot{barrier: barrier, generation: generation, accepting: true}
}

func (s *RuntimeSlot) Acquire() (*RuntimeGeneration, func(), bool) {
	s.barrier.RLock()
	s.mu.Lock()
	if !s.accepting || s.generation == nil {
		s.mu.Unlock()
		s.barrier.RUnlock()
		return nil, nil, false
	}
	if instance := core.FromContext(s.generation.Context); instance != nil && instance.RuntimeAdmissionFenced() {
		s.mu.Unlock()
		s.barrier.RUnlock()
		return nil, nil, false
	}
	generation := s.generation
	generation.mu.Lock()
	generation.sessions++
	generation.mu.Unlock()
	s.active++
	s.mu.Unlock()
	s.barrier.RUnlock()
	var once sync.Once
	return generation, func() {
		once.Do(func() {
			generation.mu.Lock()
			generation.sessions--
			close := generation.collectLocked()
			generation.mu.Unlock()
			s.mu.Lock()
			s.active--
			drain := s.takeDrainLocked()
			s.mu.Unlock()
			if close != nil {
				close()
			}
			if drain != nil {
				// Transport shutdown may wait for this worker to finish. Running
				// it inside the worker's lease release would wait on itself.
				go drain()
			}
		})
	}, true
}

// SwapLocked is called with the instance admission barrier held exclusively.
// Returned cleanup runs after the barrier is released.
func (s *RuntimeSlot) SwapLocked(generation *RuntimeGeneration) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.generation
	if old == generation {
		return nil
	}
	if generation != nil {
		generation.addOwner()
	}
	s.generation = generation
	s.accepting = generation != nil
	if old != nil {
		return old.removeOwner()
	}
	return nil
}

func (s *RuntimeSlot) Prepare()        { s.mu.Lock(); s.accepting = false; s.mu.Unlock() }
func (s *RuntimeSlot) ActivateLocked() { s.mu.Lock(); s.accepting = true; s.mu.Unlock() }

func (s *RuntimeSlot) takeDrainLocked() func() {
	if !s.accepting && s.active == 0 && s.onDrain != nil {
		drain := s.onDrain
		s.onDrain = nil
		return drain
	}
	return nil
}

func (s *RuntimeSlot) DrainLocked(onDrain func()) func() {
	s.mu.Lock()
	s.accepting = false
	s.onDrain = onDrain
	drain := s.takeDrainLocked()
	s.mu.Unlock()
	return drain
}

func (h *AlwaysOnInboundHandler) RuntimeSlot() *RuntimeSlot { return h.runtime }
