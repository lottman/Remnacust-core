package xdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWALZeroLengthWritePreservesError(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			w := newWALWriter(ctx, &lifecycleStorage{}, "zero", paramsFromConfig(&Config{}))
			defer func() { cancel(); w.Close(); w.wait() }()
			want := io.ErrClosedPipe
			if failed {
				want = errors.New("failed WAL")
				w.fail(want)
			} else if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if n, err := w.Write(nil); n != 0 || !errors.Is(err, want) {
				t.Fatalf("Write(nil) = %d, %v; want 0, %v", n, err, want)
			}
		})
	}
}

func TestWALCleanupRetrySurvivesMissingRootList(t *testing.T) {
	l := newTestListener(t, t.TempDir())
	base := l.storage
	session := "missing-root-list"
	name := objectName(downlinkPrefix(session), 0, segSuffix)
	if err := base.Put(context.Background(), name, []byte("pending")); err != nil {
		t.Fatal(err)
	}
	want := errors.New("exact delete failed")
	var deletes int
	l.storage = &lifecycleStorage{
		list: func(ctx context.Context, prefix string) ([]Entry, error) {
			if prefix == streamsDir {
				return nil, nil
			}
			return base.List(ctx, prefix)
		},
		drop: func(ctx context.Context, target string) error {
			deletes++
			if deletes == 1 {
				return want
			}
			return base.Delete(ctx, target)
		},
	}
	l.walCleanup = map[string]bool{session: true}
	l.idleSince[session] = time.Now().Add(-2 * l.sessionTTL)
	if err := l.collect(); !errors.Is(err, want) || !l.walCleanup[session] || deletes != 1 {
		t.Fatalf("incomplete root List lost failed cleanup: err=%v retry=%v deletes=%d", err, l.walCleanup[session], deletes)
	}
	l.idleSince[session] = time.Now().Add(-2 * l.sessionTTL)
	if err := l.collect(); err != nil || l.walCleanup[session] {
		t.Fatalf("retained cleanup did not retry: err=%v retry=%v", err, l.walCleanup[session])
	}
	if _, err := base.Get(context.Background(), name); err != errNotFound {
		t.Fatalf("retry left the exact WAL object: %v", err)
	}
}

func TestWALCleanupFailureUnregistersAndRetries(t *testing.T) {
	l := newTestListener(t, t.TempDir())
	l.params = paramsFromConfig(&Config{SegmentBytes: 16384, Concurrency: 2, PollIntervalMs: 5, MaxPollIntervalMs: 10, FlushIntervalMs: 5})
	l.params.sessionTTL = 80 * time.Millisecond
	base := l.storage
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var failed atomic.Bool
	session := "failed-cleanup"
	s := &lifecycleStorage{
		put:  base.Put,
		get:  base.Get,
		list: base.List,
		drop: func(ctx context.Context, name string) error {
			if strings.HasSuffix(name, segSuffix) {
				first := false
				once.Do(func() { first = true; close(entered) })
				if first {
					select {
					case <-release:
					case <-ctx.Done():
					}
					failed.Store(true)
					return errors.New("injected exact object deletion failure")
				}
				return base.Delete(ctx, name)
			}
			return nil
		},
	}
	l.storage = s
	c := l.newSessionConn(context.Background(), session, nil)
	l.mu.Lock()
	l.active[session] = true
	l.mu.Unlock()
	if _, err := c.Write(make([]byte, 2*l.segmentBytes)); err != nil {
		t.Fatal(err)
	}
	lifecycleAwait(t, entered)
	lockFree := make(chan struct{})
	go func() {
		l.mu.Lock()
		active := l.active[session]
		_, registered := l.connections[c]
		pending := l.walCleanup[session]
		l.mu.Unlock()
		c.writer.(*walWriter).mu.Lock()
		c.writer.(*walWriter).mu.Unlock()
		if active || !registered || !pending {
			t.Errorf("blocked cleanup lifecycle: active=%v joined=%v retry=%v", active, registered, pending)
		}
		close(lockFree)
	}()
	select {
	case <-lockFree:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("cleanup held a listener or writer mutex during storage I/O")
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned before its cleanup settled")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	lifecycleAwait(t, closed)
	if !failed.Load() {
		t.Fatal("the exact delete failure was not injected")
	}
	l.mu.Lock()
	retry := l.walCleanup[session]
	l.idleSince[session] = time.Now().Add(-time.Second)
	l.mu.Unlock()
	if !retry {
		t.Fatal("cleanup failure lost its GC retry marker")
	}
	if err := l.collect(); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{uplinkDir, downlinkDir} {
		entries, err := base.List(context.Background(), sessionPrefix(session)+"/"+direction)
		if err != nil || len(entries) != 0 {
			t.Fatalf("exact-only backend kept WAL entries after GC retry: %v %v", entries, err)
		}
	}
	l.mu.Lock()
	retry = l.walCleanup[session]
	l.mu.Unlock()
	if retry {
		t.Fatal("successful GC retained its retry marker")
	}
}

func TestWALCleanupPreservesForeignObjects(t *testing.T) {
	s, err := newLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	known := objectName(uplinkPrefix("owned"), 0, segSuffix)
	foreign := []string{uplinkPrefix("owned") + "/foreign.keep", sessionPrefix("owned") + "/foreign.root", objectName(uplinkPrefix("owned-sibling"), 0, segSuffix)}
	for _, name := range append([]string{known}, foreign...) {
		if err := s.Put(ctx, name, []byte("keep")); err != nil {
			t.Fatal(err)
		}
	}
	if err := collectWALSession(ctx, s, "owned"); err == nil {
		t.Fatal("foreign objects were not reported")
	}
	if _, err := s.Get(ctx, known); err != errNotFound {
		t.Fatalf("known WAL segment was not reclaimed: %v", err)
	}
	for _, name := range foreign {
		if _, err := s.Get(ctx, name); err != nil {
			t.Fatalf("foreign/sibling object was removed: %s %v", name, err)
		}
	}
}

func TestWALCleanupRejectsUnsafeSessionBeforeIO(t *testing.T) {
	for _, session := range []string{"", ".", "..", "../sibling", "a/b", "a\\b", "a~b", "a:b", "a%2Fb", "a\x00b"} {
		t.Run(fmt.Sprintf("%q", session), func(t *testing.T) {
			var calls atomic.Int32
			s := &lifecycleStorage{
				list: func(context.Context, string) ([]Entry, error) { calls.Add(1); return nil, nil },
				drop: func(context.Context, string) error { calls.Add(1); return nil },
			}
			if err := collectWALSession(context.Background(), s, session); err == nil || calls.Load() != 0 {
				t.Fatalf("unsafe session reached storage: err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestWALCleanupRejectsForeignEntryAndBoundsBatch(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			var deleted []string
			s := &lifecycleStorage{
				list: func(_ context.Context, prefix string) ([]Entry, error) {
					switch prefix {
					case sessionPrefix("owned"):
						return []Entry{{Name: uplinkDir}}, nil
					case uplinkPrefix("owned"):
						if batch {
							entries := make([]Entry, 1000)
							for i := range entries {
								entries[i].Name = fmt.Sprintf("%09d.seg", i)
							}
							return entries, nil
						}
						return []Entry{{Name: "../sibling/000000000.seg"}, {Name: "0.seg/../../sibling"}, {Name: "foreign"}, {Name: "000000001.seg"}}, nil
					}
					return nil, nil
				},
				drop: func(_ context.Context, name string) error { deleted = append(deleted, name); return nil },
			}
			if err := collectWALSession(context.Background(), s, "owned"); err == nil {
				t.Fatal("unsafe/incomplete cleanup returned success")
			}
			if batch && len(deleted) != 2*maxConcurrency+2 || !batch && len(deleted) != 1 {
				t.Fatalf("incorrect cleanup batch: %d", len(deleted))
			}
			for _, name := range deleted {
				if !strings.HasPrefix(name, uplinkPrefix("owned")+"/") || strings.Contains(name, "..") {
					t.Fatalf("cleanup issued a broad/foreign deletion: %s", name)
				}
			}
		})
	}
}

func TestWALDeletionFailureDoesNotReleaseWindow(t *testing.T) {
	base, err := newLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &walDeletionStorage{Storage: base}
	s.failures.Store(1000000)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := paramsFromConfig(&Config{SegmentBytes: 16384, Concurrency: 2, PollIntervalMs: 5, MaxPollIntervalMs: 10, FlushIntervalMs: 5})
	p.sessionTTL = 120 * time.Millisecond
	w := newWALWriter(ctx, s, "failed-delete", p)
	r := newWALReader(ctx, s, "failed-delete", p)
	defer func() { cancel(); w.Close(); w.wait(); r.wait() }()
	drained := make(chan struct{})
	go func() {
		for range r.chunks() {
		}
		close(drained)
	}()
	n, err := w.Write(make([]byte, 32*p.segmentBytes))
	if err == nil || n > 3*p.segmentBytes || s.deleted.Load() != 0 {
		t.Fatalf("failed Delete credited as consumed: n=%d err=%v deleted=%d", n, err, s.deleted.Load())
	}
	cancel()
	lifecycleAwait(t, drained)
}

func TestWALCreditRequiresCompletedPut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var puts, gets atomic.Int32
	s := &lifecycleStorage{
		put: func(ctx context.Context, _ string, _ []byte) error {
			puts.Add(1)
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		get: func(context.Context, string) ([]byte, error) { gets.Add(1); return nil, errNotFound },
	}
	p := paramsFromConfig(&Config{SegmentBytes: 16384, Concurrency: 2, PollIntervalMs: 5, MaxPollIntervalMs: 10})
	w := newWALWriter(ctx, s, "not-published", p)
	defer func() { cancel(); w.Close(); w.wait() }()
	done := make(chan error, 1)
	go func() { _, err := w.Write(make([]byte, 32*p.segmentBytes)); done <- err }()
	lifecycleAwait(t, entered)
	time.Sleep(50 * time.Millisecond)
	w.mu.Lock()
	w.mu.Unlock()
	if puts.Load() > 2 || gets.Load() != 0 {
		t.Errorf("uncommitted Put reclaimed: puts=%d gets=%d", puts.Load(), gets.Load())
	}
	select {
	case err := <-done:
		t.Errorf("uncommitted window did not backpressure: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if gets.Load() == 0 {
		t.Fatal("published missing segments were never checked")
	}
}
