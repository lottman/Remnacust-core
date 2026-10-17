package inbound

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

func TestRuntimeManifestFenceRejectsNewLeasesAndCachedStreamAdmission(t *testing.T) {
	owner, err := core.NewProfile(context.Background(), &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	generation := NewRuntimeGeneration(owner.RuntimeContext(), nil, nil)
	slot := NewRuntimeSlot(owner.RuntimeAccess(), generation)
	_, release, ok := slot.Acquire()
	if !ok {
		t.Fatal("initial lease rejected")
	}
	defer release()
	owner.FenceRuntimeAdmission()
	if _, _, ok := slot.Acquire(); ok {
		t.Fatal("fenced manifest admitted a new transport lease")
	}
	admission := &runtimeAuthDispatcher{barrier: owner.RuntimeAccess()}
	if release, err := admission.admit(owner.RuntimeContext()); err == nil {
		if release != nil {
			release()
		}
		t.Fatal("a cached transport could admit a new stream after the manifest fence")
	}
	if generation.closed || generation.sessions != 1 {
		t.Fatal("manifest fence destroyed the already pinned generation")
	}
}

func TestRuntimeGenerationPinsSessionsAndClosesExactlyOnce(t *testing.T) {
	barrier := new(sync.RWMutex)
	var closed atomic.Int32
	old := NewRuntimeGeneration(context.Background(), nil, func() { closed.Add(1) })
	slot := NewRuntimeSlot(barrier, old)
	pinned, release, ok := slot.Acquire()
	if !ok || pinned != old {
		t.Fatal("old session was not pinned")
	}
	next := NewRuntimeGeneration(context.Background(), nil, nil)
	barrier.Lock()
	cleanup := slot.SwapLocked(next)
	barrier.Unlock()
	if cleanup != nil {
		cleanup()
	}
	if closed.Load() != 0 {
		t.Fatal("active routing resources were closed")
	}
	current, done, ok := slot.Acquire()
	if !ok || current != next {
		t.Fatal("new session used old routing")
	}
	done()
	release()
	release()
	if closed.Load() != 1 {
		t.Fatal("old resources were not released exactly once")
	}
}

func TestRuntimeDrainIsPerPortAndRejectsOnlyNewSessions(t *testing.T) {
	barrier := new(sync.RWMutex)
	generation := NewRuntimeGeneration(context.Background(), nil, nil)
	first, second := NewRuntimeSlot(barrier, generation), NewRuntimeSlot(barrier, generation)
	_, releaseFirst, _ := first.Acquire()
	_, releaseSecond, _ := second.Acquire()
	var drained atomic.Bool
	done := make(chan struct{})
	barrier.Lock()
	close := first.DrainLocked(func() { drained.Store(true); close(done) })
	barrier.Unlock()
	if close != nil || drained.Load() {
		t.Fatal("port drained before its session ended")
	}
	if _, _, ok := first.Acquire(); ok {
		t.Fatal("retired port admitted a new session")
	}
	_, another, ok := second.Acquire()
	if !ok {
		t.Fatal("unrelated port stopped admitting sessions")
	}
	releaseFirst()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain incorrectly waited for another port")
	}
	another()
	releaseSecond()
}

func TestRuntimeDrainDoesNotWaitOnTheReleasingWorker(t *testing.T) {
	barrier := new(sync.RWMutex)
	slot := NewRuntimeSlot(barrier, NewRuntimeGeneration(context.Background(), nil, nil))
	_, release, _ := slot.Acquire()
	workerExited, drained := make(chan struct{}), make(chan struct{})
	barrier.Lock()
	slot.DrainLocked(func() { <-workerExited; close(drained) })
	barrier.Unlock()
	go func() { release(); close(workerExited) }()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("transport shutdown waited on its own worker")
	}
}

func TestRuntimeConcurrentAdmissionsAndCommits(t *testing.T) {
	barrier := new(sync.RWMutex)
	slot := NewRuntimeSlot(barrier, NewRuntimeGeneration(context.Background(), nil, nil))
	var workers sync.WaitGroup
	for n := 0; n < 8; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 1000; j++ {
				_, release, ok := slot.Acquire()
				if ok {
					release()
				}
			}
		}()
	}
	for j := 0; j < 100; j++ {
		barrier.Lock()
		close := slot.SwapLocked(NewRuntimeGeneration(context.Background(), nil, nil))
		barrier.Unlock()
		if close != nil {
			close()
		}
	}
	workers.Wait()
}
