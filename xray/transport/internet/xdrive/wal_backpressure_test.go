package xdrive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet/stat"
)

func walStored(t *testing.T, root string) (int64, int) {
	t.Helper()
	var size int64
	var count int
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(name, segSuffix) {
			info, err := entry.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			size += info.Size()
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return size, count
}

func TestWALPeerProcess(t *testing.T) {
	folder := os.Getenv("REMNACUST_WAL_PEER_TEST_FOLDER")
	if folder == "" {
		return
	}
	client, err := Dial(context.Background(), net.Destination{}, settings(folder))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, client, "ready")
	if err := os.WriteFile(filepath.Join(folder, "peer-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func TestWALDisappearingPeerBoundsAndCollects(t *testing.T) {
	folder := t.TempDir()
	stream := settings(folder)
	stream.ProtocolSettings.(*Config).SessionTtlSeconds = 1
	accepted := make(chan stat.Connection, 2)
	wireListener, err := Serve(context.Background(), net.LocalHostIP, 0, stream, func(c stat.Connection) { accepted <- c })
	if err != nil {
		t.Fatal(err)
	}
	l := wireListener.(*Listener)
	defer l.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestWALPeerProcess$", "-test.timeout=40s")
	child.Env = append(os.Environ(), "REMNACUST_WAL_PEER_TEST_FOLDER="+folder)
	var childLog bytes.Buffer
	child.Stdout, child.Stderr = &childLog, &childLog
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	}()
	var server *Conn
	select {
	case c := <-accepted:
		server = c.(*Conn)
	case <-time.After(5 * time.Second):
		t.Fatal("child session was not accepted")
	}
	expectRead(t, server, "hello")
	if _, err := server.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "peer handshake", func() bool { _, err := os.Stat(filepath.Join(folder, "peer-ready")); return err == nil })
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	other, err := Dial(context.Background(), net.Destination{}, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var sibling *Conn
	select {
	case c := <-accepted:
		sibling = c.(*Conn)
	case <-time.After(5 * time.Second):
		t.Fatal("sibling session was not accepted")
	}
	defer sibling.Close()
	siblingName := sibling.writer.(*walWriter).prefix + "/keep.sentinel"
	if err := l.storage.Put(context.Background(), siblingName, []byte("sibling")); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, 32*1024*1024)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() { n, err := server.Write(payload); done <- result{n, err} }()
	var peak int64
	var got result
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case got = <-done:
			break loop
		case <-tick.C:
			size, _ := walStored(t, folder)
			peak = max(peak, size)
		case <-deadline.C:
			t.Fatal("stalled Write did not fail within the session TTL budget")
		}
	}
	if got.err == nil || got.n > 8*1024*1024+defaultSegmentBytes || peak > 8*1024*1024 {
		t.Fatalf("disappeared peer: accepted=%d err=%v peak stored=%d", got.n, got.err, peak)
	}
	waitFor(t, "failed session teardown and removal", func() bool {
		l.mu.Lock()
		_, registered := l.connections[server]
		l.mu.Unlock()
		_, err := os.Stat(filepath.Join(folder, filepath.FromSlash(strings.TrimSuffix(server.writer.(*walWriter).prefix, "/s2c"))))
		return !registered && os.IsNotExist(err)
	})
	if data, err := l.storage.Get(context.Background(), siblingName); err != nil || string(data) != "sibling" {
		t.Fatalf("cleanup affected a sibling: %q %v", data, err)
	}
	if _, err := server.Write([]byte("late")); err == nil {
		t.Fatal("failed connection accepted another write")
	}
	t.Logf("abrupt peer exit: accepted=%d/33554432 peak stored=%d, session removed and sibling retained", got.n, peak)
}

func TestWALWriterBoundsStagingAndUploads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	entered := make(chan struct{})
	var once sync.Once
	s := &lifecycleStorage{put: func(ctx context.Context, _ string, _ []byte) error {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}}
	p := paramsFromConfig(&Config{SessionTtlSeconds: 1, PollIntervalMs: 5, MaxPollIntervalMs: 10})
	before := runtime.NumGoroutine()
	w := newWALWriter(ctx, s, "bounded", p)
	defer func() { cancel(); w.Close(); w.wait() }()
	done := make(chan struct{})
	go func() { defer close(done); w.Write(make([]byte, 32*1024*1024)) }()
	lifecycleAwait(t, entered)
	time.Sleep(40 * time.Millisecond)
	w.mu.Lock()
	staging := cap(w.buf)
	w.mu.Unlock()
	growth := runtime.NumGoroutine() - before
	activeUploads := calls.Load()
	if staging > 2*p.segmentBytes || growth > p.concurrency+12 || activeUploads > int32(p.concurrency) {
		t.Errorf("32MiB Write: staging=%d goroutine growth=%d uploads=%d", staging, growth, activeUploads)
	}
	select {
	case <-done:
		t.Error("a full window did not backpressure the writer")
	default:
	}
	cancel()
	lifecycleAwait(t, done)
	t.Logf("32MiB Write: staging capacity=%d goroutine growth=%d uploads=%d", staging, growth, activeUploads)
}

func TestWALHealthyProgressAndLongIdle(t *testing.T) {
	stream := settings(t.TempDir())
	stream.ProtocolSettings.(*Config).SessionTtlSeconds = 1
	client, server, cleanup := pairWith(t, stream)
	defer cleanup()
	payload := bytes.Repeat([]byte("healthy-wal"), 2*1024*1024)
	done := make(chan error, 1)
	go func() { _, err := client.Write(payload); done <- err }()
	server.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(payload))
	for n := 0; n < len(got); {
		count, err := server.Read(got[n:min(n+128*1024, len(got))])
		if err != nil {
			t.Fatal(err)
		}
		n += count
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("continuous reader: write=%v equal=%v", err, bytes.Equal(got, payload))
	}
	folder := stream.ProtocolSettings.(*Config).RemoteFolder
	waitFor(t, "all segments to be reclaimed", func() bool { size, _ := walStored(t, folder); return size == 0 })
	time.Sleep(1300 * time.Millisecond)
	if _, err := client.Write([]byte("after idle")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, server, "after idle")
	if _, err := server.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, client, "reply")
}

type walDeletionStorage struct {
	Storage
	failures atomic.Int32
	deleted  atomic.Int32
	hideList bool
	getError error
}

func (s *walDeletionStorage) Delete(ctx context.Context, name string) error {
	if strings.HasSuffix(name, segSuffix) && s.failures.Add(-1) >= 0 {
		return errors.New("temporary delete failure")
	}
	if err := s.Storage.Delete(ctx, name); err != nil {
		return err
	}
	s.deleted.Add(1)
	return nil
}

func (s *walDeletionStorage) List(ctx context.Context, prefix string) ([]Entry, error) {
	if s.hideList {
		return nil, nil
	}
	return s.Storage.List(ctx, prefix)
}

func (s *walDeletionStorage) Get(ctx context.Context, name string) ([]byte, error) {
	if s.getError != nil {
		return nil, s.getError
	}
	return s.Storage.Get(ctx, name)
}

func TestWALReaderRetriesDeletion(t *testing.T) {
	base, err := newLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &walDeletionStorage{Storage: base}
	s.failures.Store(12)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := paramsFromConfig(&Config{SegmentBytes: 16384, Concurrency: 2, PollIntervalMs: 5, MaxPollIntervalMs: 10, FlushIntervalMs: 5, SessionTtlSeconds: 1})
	w := newWALWriter(ctx, s, "retry", p)
	r := newWALReader(ctx, s, "retry", p)
	defer func() { cancel(); w.Close(); w.wait(); r.wait() }()
	payload := bytes.Repeat([]byte{0x61}, 64*p.segmentBytes)
	done := make(chan error, 1)
	go func() {
		_, err := w.Write(payload)
		if err == nil {
			err = w.Close()
		}
		done <- err
	}()
	var got []byte
	for chunk := range r.chunks() {
		got = append(got, chunk...)
	}
	if err := <-done; err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("retry transfer: %v bytes=%d/%d", err, len(got), len(payload))
	}
	waitFor(t, "failed deletions to be retried", func() bool {
		entries, err := base.List(context.Background(), "retry")
		return err == nil && len(entries) == 0
	})
	if s.failures.Load() >= 0 {
		t.Fatal("delete failures were not retried")
	}
}

func TestWALMissingListIsNotAnAck(t *testing.T) {
	for _, getError := range []error{nil, errors.New("backend unavailable")} {
		t.Run("get="+toErrorString(getError), func(t *testing.T) {
			base, err := newLocalStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := &walDeletionStorage{Storage: base, hideList: true, getError: getError}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			p := paramsFromConfig(&Config{SegmentBytes: 16384, Concurrency: 2, PollIntervalMs: 5, MaxPollIntervalMs: 10})
			p.sessionTTL = 120 * time.Millisecond
			w := newWALWriter(ctx, s, "lag", p)
			defer func() { cancel(); w.Close(); w.wait() }()
			n, err := w.Write(make([]byte, 32*p.segmentBytes))
			if err == nil || n > 3*p.segmentBytes {
				t.Fatalf("missing List credited without exact not-found: n=%d err=%v", n, err)
			}
		})
	}
}

func toErrorString(err error) string {
	if err == nil {
		return "present"
	}
	return err.Error()
}

func TestWALCloseReleasesBackpressuredWrite(t *testing.T) {
	base, err := newLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := paramsFromConfig(&Config{SegmentBytes: 16384, Concurrency: 2, PollIntervalMs: 5, MaxPollIntervalMs: 10})
	c := newConn(ctx, base, "out", "in", p, nil)
	c.closeTimeout = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() { _, err := c.Write(make([]byte, 32*p.segmentBytes)); done <- err }()
	time.Sleep(40 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Write was not backpressured: %v", err)
	default:
	}
	closed := make(chan struct{})
	go func() { defer close(closed); c.Close() }()
	lifecycleAwait(t, closed)
	select {
	case err := <-done:
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("canceled Write returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close left a blocked Write")
	}
}

func TestWALDriveSegmentPeerLargeTransfer(t *testing.T) {
	drive := newFakeDrive(t)
	stream := driveSettings()
	config := stream.ProtocolSettings.(*Config)
	config.SegmentBytes, config.Concurrency = 32768, 2
	client, err := Dial(context.Background(), net.Destination{}, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	storage := newDriveBackend(t)
	defer storage.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := storage.List(ctx, sessionsDir)
	if err != nil || len(entries) != 1 || len(entries[0].Inline) != 0 || drive.count() != 1 {
		t.Fatalf("legacy announcement changed: entries=%v err=%v files=%d", entries, err, drive.count())
	}
	session, _, ok := parseAnnounce(entries[0].Name)
	if !ok {
		t.Fatal("invalid segment announcement")
	}
	payload := bytes.Repeat([]byte("legacy-segment-wire"), 65536)
	done := make(chan error, 1)
	go func() { _, err := client.Write(payload); done <- err }()
	got := make([]byte, 0, len(payload))
	for seq := int64(0); len(got) < len(payload); seq++ {
		name := objectName(uplinkPrefix(session), seq, segSuffix)
		var data []byte
		for {
			data, err = storage.Get(ctx, name)
			if err == nil {
				break
			}
			if err != errNotFound || ctx.Err() != nil {
				t.Fatalf("legacy segment %d: %v", seq, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		got = append(got, data...)
		if err := storage.Delete(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil || !bytes.Equal(payload, got) {
		t.Fatalf("legacy segment exchange: %v equal=%v", err, bytes.Equal(payload, got))
	}
	if err := storage.Put(ctx, objectName(downlinkPrefix(session), 0, segSuffix), []byte("legacy reply")); err != nil {
		t.Fatal(err)
	}
	expectRead(t, client, "legacy reply")
}
