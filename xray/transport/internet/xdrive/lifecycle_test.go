package xdrive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
)

type lifecycleStorage struct {
	put   func(context.Context, string, []byte) error
	list  func(context.Context, string) ([]Entry, error)
	get   func(context.Context, string) ([]byte, error)
	drop  func(context.Context, string) error
	close func() error
}

func (s *lifecycleStorage) Put(ctx context.Context, name string, data []byte) error {
	if s.put != nil {
		return s.put(ctx, name, data)
	}
	return ctx.Err()
}
func (s *lifecycleStorage) List(ctx context.Context, prefix string) ([]Entry, error) {
	if s.list != nil {
		return s.list(ctx, prefix)
	}
	return nil, ctx.Err()
}
func (s *lifecycleStorage) Get(ctx context.Context, name string) ([]byte, error) {
	if s.get != nil {
		return s.get(ctx, name)
	}
	return nil, errNotFound
}
func (s *lifecycleStorage) Delete(ctx context.Context, name string) error {
	if s.drop != nil {
		return s.drop(ctx, name)
	}
	return ctx.Err()
}
func (s *lifecycleStorage) Close() error {
	if s.close != nil {
		return s.close()
	}
	return nil
}

type lifecycleBlocked struct {
	entered, canceled, release, exited chan struct{}
	once                               sync.Once
}

func newLifecycleBlocked() *lifecycleBlocked {
	return &lifecycleBlocked{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
}

func (b *lifecycleBlocked) run(ctx context.Context) error {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	close(b.canceled)
	<-b.release
	close(b.exited)
	return ctx.Err()
}

func lifecycleAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle operation did not finish")
	}
}

func TestConnCloseJoinsStorageWorkers(t *testing.T) {
	for _, operation := range []string{"list", "get", "delete"} {
		t.Run(operation, func(t *testing.T) {
			blocked := newLifecycleBlocked()
			var earlyClose atomic.Bool
			var listed atomic.Bool
			s := &lifecycleStorage{}
			s.list = func(ctx context.Context, _ string) ([]Entry, error) {
				if operation == "list" {
					return nil, blocked.run(ctx)
				}
				if listed.Swap(true) {
					return nil, nil
				}
				entry := Entry{Name: "000000000.seg"}
				if operation == "delete" {
					entry.Inline = []byte("payload")
				}
				return []Entry{entry}, nil
			}
			s.get = func(ctx context.Context, _ string) ([]byte, error) { return nil, blocked.run(ctx) }
			s.drop = func(ctx context.Context, _ string) error { return blocked.run(ctx) }
			s.close = func() error {
				select {
				case <-blocked.exited:
				default:
					earlyClose.Store(true)
				}
				return nil
			}
			c := newConn(context.Background(), s, "out", "in", paramsFromConfig(&Config{}), func() { s.Close() })
			lifecycleAwait(t, blocked.entered)
			done := make(chan struct{})
			go func() { defer close(done); c.Close() }()
			lifecycleAwait(t, blocked.canceled)
			select {
			case <-done:
				earlyClose.Store(true)
			case <-time.After(20 * time.Millisecond):
			}
			close(blocked.release)
			lifecycleAwait(t, done)
			if earlyClose.Load() {
				t.Fatal("Close released storage before the canceled worker finished")
			}
		})
	}
}

func lifecycleListener(t *testing.T, s Storage, handler internet.ConnHandler) *Listener {
	t.Helper()
	factory := func(*Config) (Storage, error) { return s, nil }
	old := testStorageFactory.Swap(&factory)
	t.Cleanup(func() { testStorageFactory.Store(old) })
	l, err := Serve(context.Background(), net.LocalHostIP, 0, settings("lifecycle-fixture"), handler)
	if err != nil {
		t.Fatal(err)
	}
	return l.(*Listener)
}

func lifecycleAnnouncement() Entry {
	return Entry{Name: strings.TrimPrefix(announceName("00000000000000000000000000000000", time.Now()), sessionsDir+"/")}
}

func TestListenerCloseJoinsAnnouncementDeletion(t *testing.T) {
	blocked := newLifecycleBlocked()
	var listed, earlyClose atomic.Bool
	s := &lifecycleStorage{}
	s.list = func(ctx context.Context, prefix string) ([]Entry, error) {
		if prefix == sessionsDir && !listed.Swap(true) {
			return []Entry{lifecycleAnnouncement()}, nil
		}
		return nil, ctx.Err()
	}
	s.drop = func(ctx context.Context, _ string) error { return blocked.run(ctx) }
	s.close = func() error {
		select {
		case <-blocked.exited:
		default:
			earlyClose.Store(true)
		}
		return nil
	}
	accepted := make(chan stat.Connection, 1)
	l := lifecycleListener(t, s, func(c stat.Connection) { accepted <- c })
	lifecycleAwait(t, blocked.entered)
	var c stat.Connection
	select {
	case c = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("connection not delivered")
	}
	done := make(chan struct{})
	go func() { defer close(done); l.Close() }()
	lifecycleAwait(t, blocked.canceled)
	select {
	case <-done:
		earlyClose.Store(true)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocked.release)
	lifecycleAwait(t, done)
	c.Close()
	if earlyClose.Load() {
		t.Fatal("listener released storage before its deletion worker finished")
	}
}

func TestListenerCloseFromConnectionHandler(t *testing.T) {
	var listed atomic.Bool
	s := &lifecycleStorage{list: func(ctx context.Context, prefix string) ([]Entry, error) {
		if prefix == sessionsDir && !listed.Swap(true) {
			return []Entry{lifecycleAnnouncement()}, nil
		}
		return nil, ctx.Err()
	}}
	ready, done := make(chan struct{}), make(chan struct{})
	var l *Listener
	l = lifecycleListener(t, s, func(c stat.Connection) { <-ready; l.Close(); c.Close(); close(done) })
	close(ready)
	lifecycleAwait(t, done)
}

func TestListenerCloseJoinsAcceptedSession(t *testing.T) {
	blocked := newLifecycleBlocked()
	var listed, earlyClose atomic.Bool
	s := &lifecycleStorage{
		list: func(ctx context.Context, prefix string) ([]Entry, error) {
			if prefix == sessionsDir && !listed.Swap(true) {
				return []Entry{lifecycleAnnouncement()}, nil
			}
			if strings.HasPrefix(prefix, streamsDir+"/") {
				return nil, blocked.run(ctx)
			}
			return nil, ctx.Err()
		},
		close: func() error {
			select {
			case <-blocked.exited:
			default:
				earlyClose.Store(true)
			}
			return nil
		},
	}
	accepted := make(chan stat.Connection, 1)
	l := lifecycleListener(t, s, func(c stat.Connection) { accepted <- c })
	lifecycleAwait(t, blocked.entered)
	var c stat.Connection
	select {
	case c = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("session was not delivered")
	}
	done := make(chan struct{})
	go func() { defer close(done); l.Close() }()
	lifecycleAwait(t, blocked.canceled)
	select {
	case <-done:
		earlyClose.Store(true)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocked.release)
	lifecycleAwait(t, done)
	c.Close()
	if earlyClose.Load() {
		t.Fatal("listener closed shared storage before its accepted session worker finished")
	}
}

func TestConnCloseAfterReaderFailure(t *testing.T) {
	want := errors.New("storage read failed")
	s := &lifecycleStorage{list: func(context.Context, string) ([]Entry, error) { return nil, want }}
	c := newConn(context.Background(), s, "out", "in", paramsFromConfig(&Config{}), nil)
	select {
	case <-c.reader.chunks():
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not report its failure")
	}
	if !errors.Is(c.reader.Err(), want) {
		t.Fatal("reader failure was lost")
	}
	done := make(chan struct{})
	go func() { defer close(done); c.Close() }()
	lifecycleAwait(t, done)
}

func TestConnCloseHonorsStorageDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s := &lifecycleStorage{
		put:  func(ctx context.Context, _ string, _ []byte) error { <-ctx.Done(); return ctx.Err() },
		list: func(ctx context.Context, _ string) ([]Entry, error) { <-ctx.Done(); return nil, ctx.Err() },
	}
	c := newConn(ctx, s, "out", "in", paramsFromConfig(&Config{}), nil)
	done := make(chan struct{})
	var closeErr error
	go func() { defer close(done); closeErr = c.Close() }()
	lifecycleAwait(t, done)
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close gave %v, want storage deadline", closeErr)
	}
}

func TestConnCloseOwnBudget(t *testing.T) {
	entered := make(chan struct{})
	s := &lifecycleStorage{put: func(ctx context.Context, _ string, _ []byte) error { close(entered); <-ctx.Done(); return ctx.Err() }}
	c := newConn(context.Background(), s, "out", "in", paramsFromConfig(&Config{}), nil)
	if c.closeTimeout != closeBudget || c.closeTimeout <= 0 {
		t.Fatal("missing finite default close budget")
	}
	c.closeTimeout = 20 * time.Millisecond
	done := make(chan struct{})
	var err error
	go func() { defer close(done); err = c.Close() }()
	lifecycleAwait(t, entered)
	lifecycleAwait(t, done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close gave %v, want its own close budget", err)
	}
}

type lifecycleSlots struct {
	*lifecycleStorage
	*memSlots
}

func TestSlotAnnouncementJoinedBeforeStorageClose(t *testing.T) {
	blocked := newLifecycleBlocked()
	var early atomic.Bool
	s := &lifecycleSlots{lifecycleStorage: &lifecycleStorage{
		put: func(ctx context.Context, _ string, _ []byte) error { return blocked.run(ctx) },
		close: func() error {
			select {
			case <-blocked.exited:
			default:
				early.Store(true)
			}
			return nil
		},
	}, memSlots: newMemSlots()}
	config := &Config{Service: "local", RemoteFolder: "announcement-fixture", UseSlots: true, Concurrency: 2}
	box := spareBoxFor(config)
	box.busy.Store(true)
	t.Cleanup(func() { spareBoxes.Delete(config) })
	factory := func(*Config) (Storage, error) { return s, nil }
	old := testStorageFactory.Swap(&factory)
	t.Cleanup(func() { testStorageFactory.Store(old) })
	wire, err := Dial(context.Background(), net.Destination{}, &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config})
	if err != nil {
		t.Fatal(err)
	}
	c := wire.(*Conn)
	c.closeTimeout = 20 * time.Millisecond
	lifecycleAwait(t, blocked.entered)
	done := make(chan struct{})
	var closeErr error
	go func() { defer close(done); closeErr = c.Close() }()
	lifecycleAwait(t, blocked.canceled)
	select {
	case <-done:
		early.Store(true)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocked.release)
	lifecycleAwait(t, done)
	if early.Load() {
		t.Fatal("storage closed before announcement completed")
	}
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close gave %v, want its own close budget", closeErr)
	}
}

func TestListenerCloseDuringBlockedHandler(t *testing.T) {
	var listed, calls atomic.Int32
	s := &lifecycleStorage{list: func(ctx context.Context, prefix string) ([]Entry, error) {
		if prefix == sessionsDir && listed.Add(1) == 1 {
			first := lifecycleAnnouncement()
			second := first
			second.Name = strings.Replace(first.Name, "00000000000000000000000000000000", "11111111111111111111111111111111", 1)
			return []Entry{first, second}, nil
		}
		return nil, ctx.Err()
	}}
	entered, release, returned, unexpected := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	l := lifecycleListener(t, s, func(c stat.Connection) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			c.Close()
			close(returned)
		} else {
			c.Close()
			close(unexpected)
		}
	})
	lifecycleAwait(t, entered)
	closed := make(chan struct{})
	go func() { defer close(closed); l.Close() }()
	lifecycleAwait(t, closed)
	if _, err := l.acceptPending(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("accept after Close gave %v", err)
	}
	close(release)
	lifecycleAwait(t, returned)
	select {
	case <-unexpected:
		t.Fatal("listener delivered another connection after Close")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLocalStorageCanceledOperations(t *testing.T) {
	root := t.TempDir()
	s, err := newLocalStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Put(context.Background(), "kept", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() error{
		"put":    func() error { return s.Put(ctx, "new/object", nil) },
		"get":    func() error { _, err := s.Get(ctx, "kept"); return err },
		"delete": func() error { return s.Delete(ctx, "kept") },
		"list":   func() error { _, err := s.List(ctx, "kept"); return err },
	} {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s gave %v", name, err)
		}
	}
	if _, err = os.Stat(filepath.Join(root, "kept")); err != nil {
		t.Fatal("canceled Delete removed the object", err)
	}
	if _, err = os.Stat(filepath.Join(root, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled Put created a directory", err)
	}
}
