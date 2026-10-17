package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lifetimeClosingFeature struct {
	parent  *Instance
	scope   *FeatureScope
	calls   atomic.Int32
	failure error
}

func (*lifetimeClosingFeature) Type() interface{} { return (*lifetimeClosingFeature)(nil) }
func (*lifetimeClosingFeature) Start() error      { return nil }
func (f *lifetimeClosingFeature) Close() error {
	f.calls.Add(1)
	// QUIC transport cleanup can wait for a worker that checks either lifetime.
	if f.parent.IsRunning() || f.scope.Instance.IsRunning() {
		return errors.New("closing parent is still running")
	}
	return f.failure
}

func TestShutdownAllowsLifetimeChecksAndClosesFeaturesOnce(t *testing.T) {
	parent := &Instance{ctx: context.Background()}
	scope := NewFeatureScope(parent)
	feature := &lifetimeClosingFeature{parent: parent, scope: scope, failure: errors.New("close failure")}
	if err := parent.AddFeature(feature); err != nil {
		t.Fatal(err)
	}
	if err := scope.Start(); err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	parent.BeforeClose(func() {
		if parent.IsRunning() || scope.Instance.IsRunning() {
			t.Error("shutdown callback observed live parent")
		}
	})
	results := make(chan error, 16)
	var workers sync.WaitGroup
	for range cap(results) {
		workers.Add(1)
		go func() { defer workers.Done(); results <- parent.Close() }()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(results); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown deadlocked a feature or callback checking IsRunning")
	}
	if feature.calls.Load() != 1 {
		t.Fatalf("concurrent Close ran teardown %d times", feature.calls.Load())
	}
	var firstError error
	for err := range results {
		if err == nil || !strings.Contains(err.Error(), feature.failure.Error()) {
			t.Fatalf("Close lost its failure: %v", err)
		}
		if firstError != nil && err != firstError {
			t.Fatal("concurrent Close did not return the same teardown result")
		}
		firstError = err
	}
}
