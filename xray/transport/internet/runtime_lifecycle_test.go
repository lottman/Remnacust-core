package internet

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRuntimeTransportRetirementRunsEachHookOnceOutsideItsLock(t *testing.T) {
	stream := new(MemoryStreamConfig)
	var registered, closed atomic.Int64
	var workers sync.WaitGroup
	for i := 0; i < 100; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if stream.AddCloseHook(func() {
				if stream.AddCloseHook(func() { t.Error("hook registered after retirement") }) {
					t.Error("retired configuration accepted a callback")
				}
				closed.Add(1)
			}) {
				registered.Add(1)
			}
		}()
	}
	workers.Add(1)
	go func() { defer workers.Done(); stream.Close() }()
	workers.Wait()
	stream.Close()
	if !stream.IsClosed() || registered.Load() != closed.Load() {
		t.Fatalf("transport hooks leaked or ran twice: registered=%d closed=%d", registered.Load(), closed.Load())
	}
}

func TestRuntimeTransportRetiresSeparateDownloadConfiguration(t *testing.T) {
	download := new(MemoryStreamConfig)
	upload := &MemoryStreamConfig{DownloadSettings: download}
	var closed atomic.Int64
	download.AddCloseHook(func() { closed.Add(1) })
	upload.Close()
	upload.Close()
	if !download.IsClosed() || closed.Load() != 1 {
		t.Fatal("separate download pool was not retired exactly once")
	}
}
