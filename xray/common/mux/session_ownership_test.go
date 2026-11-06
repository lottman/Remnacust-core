package mux

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestDuplicateSessionIDPreservesOriginalLease(t *testing.T) {
	m := NewSessionManager()
	first := m.Allocate(&ClientStrategy{}, nil, nil)
	var released, rejectedReleased int
	first.release = func() { released++ }
	duplicate := &Session{ID: first.ID, parent: m, release: func() { rejectedReleased++ }}
	if m.Add(duplicate) {
		t.Fatal("duplicate session replaced its live owner")
	}
	duplicate.Close(false)
	if current, ok := m.Get(first.ID); !ok || current != first || released != 0 || rejectedReleased != 1 {
		t.Fatal("rejected session teardown affected the live owner")
	}
	if m.Count() != 1 || m.Size() != 1 {
		t.Fatal("rejected session changed manager accounting")
	}
	m.Close()
	first.Close(false)
	duplicate.Close(false)
	if released != 1 || rejectedReleased != 1 {
		t.Fatalf("lease releases: original=%d rejected=%d", released, rejectedReleased)
	}
}

func TestUnregisteredSessionCloseCannotRemoveAnotherOwner(t *testing.T) {
	m := NewSessionManager()
	first := m.Allocate(&ClientStrategy{}, nil, nil)
	orphan := &Session{ID: first.ID, parent: m}
	orphan.Close(false)
	if current, ok := m.Get(first.ID); !ok || current != first {
		t.Fatal("closing a different session removed the registered owner")
	}
	m.Close()
}

func TestSessionIDCanBeReusedAfterItsOwnerCloses(t *testing.T) {
	m := NewSessionManager()
	first := m.Allocate(&ClientStrategy{}, nil, nil)
	first.Close(false)
	var released int
	next := &Session{ID: first.ID, parent: m, release: func() { released++ }}
	if !m.Add(next) {
		t.Fatal("completed session ID could not be reused")
	}
	first.Close(false)
	if current, ok := m.Get(next.ID); !ok || current != next {
		t.Fatal("old owner teardown removed the replacement")
	}
	m.Close()
	if released != 1 {
		t.Fatalf("replacement lease released %d times", released)
	}
}

func TestConcurrentDuplicateSessionsReleaseEachLeaseOnce(t *testing.T) {
	m := NewSessionManager()
	var accepted, released atomic.Int32
	var workers sync.WaitGroup
	for range 256 {
		workers.Go(func() {
			s := &Session{ID: 9, parent: m, release: func() { released.Add(1) }}
			if m.Add(s) {
				accepted.Add(1)
			} else {
				s.Close(false)
			}
		})
	}
	workers.Wait()
	if accepted.Load() != 1 || m.Size() != 1 || m.Count() != 1 {
		t.Fatalf("session ownership: accepted=%d size=%d count=%d", accepted.Load(), m.Size(), m.Count())
	}
	m.Close()
	if released.Load() != 256 {
		t.Fatalf("released %d leases, expected 256", released.Load())
	}
}
