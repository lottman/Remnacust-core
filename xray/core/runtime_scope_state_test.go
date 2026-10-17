package core

import (
	"context"
	"testing"
)

// Hysteria's upstream client cleanup checks its owning Instance. A live or
// draining generation must stay running until its actual teardown.
func TestRuntimeScopeReportsItsOwnAndParentLifetime(t *testing.T) {
	parent := &Instance{ctx: context.Background(), running: true}
	scope := NewFeatureScope(parent)
	if scope.Instance.IsRunning() {
		t.Fatal("unstarted generation is running")
	}
	if err := scope.Start(); err != nil {
		t.Fatal(err)
	}
	if !scope.Instance.IsRunning() {
		t.Fatal("live generation would be closed by transport cleanup")
	}
	newer := NewFeatureScope(parent)
	if err := newer.Start(); err != nil {
		t.Fatal(err)
	}
	if !scope.Instance.IsRunning() || !newer.Instance.IsRunning() {
		t.Fatal("creating a newer generation retired a still-draining generation")
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if scope.Instance.IsRunning() || !newer.Instance.IsRunning() || !parent.IsRunning() {
		t.Fatal("generation teardown changed another instance's lifetime")
	}
	parent.statusLock.Lock()
	parent.running = false
	parent.statusLock.Unlock()
	if newer.Instance.IsRunning() {
		t.Fatal("generation stayed running after its parent stopped")
	}
	_ = newer.Close()
}
