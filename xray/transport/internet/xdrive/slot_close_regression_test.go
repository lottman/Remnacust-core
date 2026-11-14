package xdrive

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type closeAdmissionContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *closeAdmissionContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

type closeAdmissionSlots struct {
	*memSlots
	closed atomic.Bool
	late   atomic.Int32
}

func (s *closeAdmissionSlots) setSlot(ctx context.Context, id, description string, body []byte) error {
	if s.closed.Load() {
		s.late.Add(1)
	}
	return s.memSlots.setSlot(ctx, id, description, body)
}

func TestSlotPausedProducerCannotUploadAfterClose(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "closed"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &closeAdmissionContext{Context: ctx, entered: make(chan struct{})}
			store := &closeAdmissionSlots{memSlots: newMemSlots()}
			store.descs["slot"] = ""
			w := &slotWriter{ctx: observed, store: store, table: slotTable{Slots: []string{"slot"}}, params: params{segmentBytes: 64}, slots: make(chan struct{}, 1), size: 1, acked: -1, progress: time.Now()}
			w.slots <- struct{}{}
			done := make(chan struct{})
			var writeErr error
			go func() { defer close(done); _, writeErr = w.Write([]byte("pending")) }()
			lifecycleAwait(t, observed.entered)
			w.mu.Lock()
			<-w.slots
			waitFor(t, "producer to reserve the released slot", func() bool { return len(w.slots) == 1 })
			w.closed = true
			if canceled {
				cancel()
			}
			w.wg.Wait()
			store.closed.Store(true)
			w.mu.Unlock()
			lifecycleAwait(t, done)
			w.wg.Wait()
			if store.late.Load() != 0 || w.seq != 0 {
				t.Fatalf("paused producer admitted %d uploads after close, seq=%d", store.late.Load(), w.seq)
			}
			if canceled && !errors.Is(writeErr, context.Canceled) {
				t.Fatalf("Write gave %v, want cancellation", writeErr)
			}
			if !canceled && !errors.Is(writeErr, io.ErrClosedPipe) {
				t.Fatalf("Write gave %v, want closed pipe", writeErr)
			}
			if len(w.slots) != 0 {
				t.Fatal("rejected producer leaked its slot reservation")
			}
		})
	}
}

func TestSlotGracefulCloseFlushesBufferedData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := newMemSlots()
	store.descs["first"] = ""
	store.descs["second"] = ""
	w := newSlotWriter(ctx, store, slotTable{Slots: []string{"first", "second"}}, params{segmentBytes: 64, flushInterval: time.Hour})
	defer func() { cancel(); w.wait() }()
	if _, err := w.Write([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	first, second := store.descs["first"], store.descs["second"]
	store.mu.Unlock()
	data, ok := decodeSlot(first)
	if !ok || data.seq != 0 || data.mode != slotInline || string(data.data) != "pending" {
		t.Fatal("Close did not preserve buffered data")
	}
	end, ok := decodeSlot(second)
	if !ok || end.seq != 1 || end.mode != slotEnd {
		t.Fatal("Close did not publish EOF after data")
	}
}

func TestConnCloseBudgetIncludesCanceledReaderJoin(t *testing.T) {
	blocked := newLifecycleBlocked()
	var early atomic.Bool
	store := &lifecycleStorage{
		list: func(ctx context.Context, _ string) ([]Entry, error) { return nil, blocked.run(ctx) },
		close: func() error {
			select {
			case <-blocked.exited:
			default:
				early.Store(true)
			}
			return nil
		},
	}
	c := newConn(context.Background(), store, "out", "in", paramsFromConfig(&Config{}), func() { store.Close() })
	c.closeTimeout = 20 * time.Millisecond
	lifecycleAwait(t, blocked.entered)
	done := make(chan struct{})
	var closeErr error
	go func() { defer close(done); closeErr = c.Close() }()
	lifecycleAwait(t, blocked.canceled)
	select {
	case <-done:
		early.Store(true)
	case <-time.After(2 * c.closeTimeout):
	}
	close(blocked.release)
	lifecycleAwait(t, done)
	if early.Load() {
		t.Fatal("Close abandoned reader join or released storage early")
	}
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close gave %v after cleanup exceeded budget", closeErr)
	}
}
