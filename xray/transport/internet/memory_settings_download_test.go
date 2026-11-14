package internet

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadSettingsPublicationRejectsClosedParent(t *testing.T) {
	parent, child := new(MemoryStreamConfig), new(MemoryStreamConfig)
	var closed atomic.Int32
	child.AddCloseHook(func() { closed.Add(1) })
	parent.Close()
	actual, err := parent.LoadOrStoreDownloadSettings(child)
	if actual != nil || !errors.Is(err, net.ErrClosed) || !child.IsClosed() || closed.Load() != 1 || parent.DownloadSettings != nil {
		t.Fatalf("actual=%p err=%v childClosed=%t cleanup=%d", actual, err, child.IsClosed(), closed.Load())
	}
	if _, err := parent.GetDownloadSettings(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed getter: %v", err)
	}
}

func TestDownloadSettingsPublicationKeepsWinner(t *testing.T) {
	parent, first, second := new(MemoryStreamConfig), new(MemoryStreamConfig), new(MemoryStreamConfig)
	defer parent.Close()
	actual, err := parent.LoadOrStoreDownloadSettings(first)
	if err != nil || actual != first {
		t.Fatalf("first: %p %v", actual, err)
	}
	actual, err = parent.LoadOrStoreDownloadSettings(first)
	if err != nil || actual != first || first.IsClosed() {
		t.Fatal("republishing the winner retired it")
	}
	second.AddCloseHook(func() {
		cached, getErr := parent.GetDownloadSettings()
		if getErr != nil || cached != first {
			t.Errorf("cleanup under parent lock or wrong winner: %p %v", cached, getErr)
		}
	})
	actual, err = parent.LoadOrStoreDownloadSettings(second)
	if err != nil || actual != first || !second.IsClosed() || first.IsClosed() {
		t.Fatal("loser replaced or closed the winner")
	}
	parent.Close()
	if !first.IsClosed() {
		t.Fatal("parent did not retire its published child")
	}
}

func TestDownloadSettingsPublicationRacesParentClose(t *testing.T) {
	for range 128 {
		parent, child := new(MemoryStreamConfig), new(MemoryStreamConfig)
		var cleaned atomic.Int32
		child.AddCloseHook(func() { cleaned.Add(1) })
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; parent.LoadOrStoreDownloadSettings(child) }()
		go func() { defer wg.Done(); <-start; parent.Close() }()
		close(start)
		wg.Wait()
		if !parent.IsClosed() || !child.IsClosed() || cleaned.Load() != 1 {
			t.Fatalf("orphaned child: parent=%t child=%t cleanup=%d", parent.IsClosed(), child.IsClosed(), cleaned.Load())
		}
	}
}

func TestDownloadSettingsRejectedCleanupRunsOutsideParentLock(t *testing.T) {
	parent, child := new(MemoryStreamConfig), new(MemoryStreamConfig)
	parent.Close()
	child.AddCloseHook(func() { parent.GetDownloadSettings() })
	done := make(chan struct{})
	go func() { parent.LoadOrStoreDownloadSettings(child); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("child cleanup retained parent closeAccess")
	}
}
