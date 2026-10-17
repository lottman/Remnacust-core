package core

import (
	"context"
	"testing"
)

func TestRuntimeShutdownIsSharedByDrainingScopesAndLateSubscribers(t *testing.T) {
	parent := &Instance{ctx: context.Background()}
	scope := NewFeatureScope(parent)
	done := parent.RuntimeDone()
	if scope.Instance.RuntimeDone() != done {
		t.Fatal("feature scope used a separate shutdown signal")
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("retiring a generation stopped the parent")
	default:
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("root close did not release draining waiters")
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	parent.BeforeClose(func() { called = true })
	if !called {
		t.Fatal("late shutdown callback was silently lost")
	}
	late := &Instance{}
	if err := late.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-late.RuntimeDone():
	default:
		t.Fatal("late subscriber missed shutdown")
	}
}
