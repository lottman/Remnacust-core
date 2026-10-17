package pubsub

import (
	"github.com/xtls/xray-core/common/task"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetiredServiceCannotRestartItsTaskOrRetainSubscribers(t *testing.T) {
	service := NewService()
	var runs atomic.Int64
	service.ctask = &task.Periodic{Interval: time.Hour, Execute: func() error { runs.Add(1); return nil }}
	first := service.Subscribe("before")
	if runs.Load() != 1 {
		t.Fatal("cleanup task not started")
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() { sub := service.Subscribe("racing"); service.Publish("racing", "message"); _ = sub.Close() })
	}
	_ = service.Close()
	workers.Wait()
	if !first.IsClosed() {
		t.Fatal("retired subscription remained active")
	}
	before := runs.Load()
	for range 32 {
		if !service.Subscribe("after").IsClosed() {
			t.Fatal("retired service accepted a subscriber")
		}
	}
	if runs.Load() != before {
		t.Fatal("subscription restarted retired cleanup task")
	}
	if len(service.subs) != 0 {
		t.Fatal("retired service retained subscriptions")
	}
	_ = service.Close()
}
