package xdrive

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/errors"
)

const (
	maxCoalescedTicks = 8

	segSuffix = ".seg"
	endSuffix = ".end"
	errSuffix = ".err"
)

func objectName(prefix string, seq int64, suffix string) string {
	return fmt.Sprintf("%s/%09d%s", prefix, seq, suffix)
}

func parseEntry(name string) (int64, bool) {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return 0, false
	}
	switch name[dot:] {
	case segSuffix, endSuffix, errSuffix:
	default:
		return 0, false
	}
	seq, err := strconv.ParseInt(name[:dot], 10, 64)
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}

type walWriter struct {
	ctx     context.Context
	cancel  context.CancelFunc
	storage Storage
	prefix  string
	params
	sem       chan struct{}
	wg        sync.WaitGroup
	done      chan struct{}
	reclaimed chan struct{}
	failed    chan struct{}

	writeMu  sync.Mutex
	mu       sync.Mutex
	buf      []byte
	seq      int64
	lastSize int
	held     int
	closed   bool
	closing  bool
	err      error
	window   int
	pending  map[int64]bool
	changed  chan struct{}
	progress time.Time
	stall    *time.Timer
}

func newWALWriter(ctx context.Context, storage Storage, prefix string, p params) *walWriter {
	ctx, cancel := context.WithCancel(ctx)
	w := &walWriter{
		ctx:       ctx,
		cancel:    cancel,
		storage:   storage,
		prefix:    prefix,
		params:    p,
		sem:       make(chan struct{}, p.concurrency),
		done:      make(chan struct{}),
		reclaimed: make(chan struct{}),
		failed:    make(chan struct{}),
		window:    max(1, min(p.concurrency, (8*1024*1024)/p.segmentBytes)),
		pending:   make(map[int64]bool),
		changed:   make(chan struct{}),
	}
	go w.flushLoop()
	go w.reclaimLoop()
	return w
}

func (w *walWriter) Write(p []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writeErrLocked(); err != nil {
		return 0, err
	}
	n := 0
	for len(p) > 0 {
		if err := w.writeErrLocked(); err != nil {
			return n, err
		}
		if len(w.buf) == w.segmentBytes {
			if err := w.waitWindowLocked(true); err != nil {
				return n, err
			}
			if err := w.flushLocked(); err != nil {
				return n, err
			}
		}
		if w.buf == nil {
			w.buf = make([]byte, 0, w.segmentBytes)
		}
		count := min(len(p), w.segmentBytes-len(w.buf))
		w.buf = append(w.buf, p[:count]...)
		p = p[count:]
		n += count
		if len(w.buf) == w.segmentBytes && len(w.pending) < w.window {
			if err := w.flushLocked(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (w *walWriter) writeErrLocked() error {
	if w.err != nil {
		return w.err
	}
	if w.closed || w.closing {
		return io.ErrClosedPipe
	}
	return w.ctx.Err()
}

func (w *walWriter) notifyLocked() {
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *walWriter) waitWindowLocked(writing bool) error {
	for {
		if w.err != nil {
			return w.err
		}
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if writing && (w.closed || w.closing) {
			return io.ErrClosedPipe
		}
		if len(w.pending) < w.window {
			return nil
		}
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-w.ctx.Done():
		case <-changed:
		}
		w.mu.Lock()
	}
}

func (w *walWriter) flushLoop() {
	defer func() {
		w.mu.Lock()
		if w.stall != nil {
			w.stall.Stop()
		}
		w.mu.Unlock()
		close(w.done)
	}()
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.mu.Lock()
			if !w.closed && !w.closing && w.err == nil && len(w.buf) > 0 && len(w.pending) < w.window && w.readyToFlush() {
				w.flushLocked()
			}
			done := w.closed || w.err != nil
			w.mu.Unlock()
			if done {
				return
			}
		}
	}
}

func (w *walWriter) wait() { <-w.done; <-w.reclaimed }

func (w *walWriter) failLocked(err error) {
	if w.err != nil {
		return
	}
	w.err = err
	if w.stall != nil {
		w.stall.Stop()
	}
	close(w.failed)
	w.notifyLocked()
	w.cancel()
}

func (w *walWriter) fail(err error) {
	w.mu.Lock()
	w.failLocked(err)
	w.mu.Unlock()
}

func (w *walWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *walWriter) progressLocked() {
	w.progress = time.Now()
	if len(w.pending) == 0 {
		if w.stall != nil {
			w.stall.Stop()
		}
		return
	}
	if w.stall != nil {
		w.stall.Reset(w.sessionTTL)
		return
	}
	w.stall = time.AfterFunc(w.sessionTTL, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.closed || w.err != nil || len(w.pending) == 0 {
			return
		}
		if remaining := w.sessionTTL - time.Since(w.progress); remaining > 0 {
			w.stall.Reset(remaining)
			return
		}
		w.failLocked(errors.New("WAL receiver made no progress within the session TTL"))
	})
}

func (w *walWriter) reclaimLoop() {
	defer close(w.reclaimed)
	delay := w.minPollInterval
	for {
		timer := time.NewTimer(delay)
		select {
		case <-w.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		w.mu.Lock()
		if w.closed || w.err != nil {
			w.mu.Unlock()
			return
		}
		var uploaded []int64
		for seq, done := range w.pending {
			if done {
				uploaded = append(uploaded, seq)
			}
		}
		w.mu.Unlock()
		if len(uploaded) == 0 {
			delay = w.maxPollInterval
			continue
		}
		reclaimed, err := w.reclaim(uploaded)
		if err != nil {
			if w.ctx.Err() == nil {
				w.fail(errors.New("failed to check consumed WAL segments").Base(err))
			}
			return
		}
		if reclaimed {
			delay = w.minPollInterval
		} else {
			delay = min(2*delay, w.maxPollInterval)
		}
	}
}

func (w *walWriter) reclaim(uploaded []int64) (bool, error) {
	listed, err := w.storage.List(w.ctx, w.prefix)
	if err != nil {
		return false, err
	}
	present := make(map[string]bool, len(listed))
	for _, entry := range listed {
		present[entry.Name] = true
	}
	advanced := false
	for _, seq := range uploaded {
		name := objectName(w.prefix, seq, segSuffix)
		if present[strings.TrimPrefix(name, w.prefix+"/")] {
			continue
		}
		if _, err := w.storage.Get(w.ctx, name); err == nil {
			continue
		} else if err != errNotFound {
			return advanced, err
		}
		w.mu.Lock()
		if !w.closed && w.err == nil && w.pending[seq] {
			delete(w.pending, seq)
			w.progressLocked()
			w.notifyLocked()
			advanced = true
		}
		w.mu.Unlock()
	}
	return advanced, nil
}

func (w *walWriter) readyToFlush() bool {
	grew := len(w.buf) > w.lastSize
	w.lastSize = len(w.buf)

	if grew && w.held < maxCoalescedTicks {
		w.held++
		return false
	}
	w.held = 0
	return true
}

func (w *walWriter) flushLocked() error {
	if len(w.buf) == 0 {
		return nil
	}
	if w.err != nil {
		return w.err
	}

	chunk := w.buf
	seq := w.seq
	w.seq++
	w.pending[seq] = false
	if len(w.pending) == 1 {
		w.progressLocked()
	}
	w.buf = nil
	w.lastSize = len(w.buf)
	w.held = 0

	w.wg.Add(1)
	go w.upload(seq, chunk)
	return nil
}

func (w *walWriter) upload(seq int64, chunk []byte) {
	defer w.wg.Done()

	select {
	case w.sem <- struct{}{}:
	case <-w.ctx.Done():
		return
	}
	defer func() { <-w.sem }()

	if err := w.storage.Put(w.ctx, objectName(w.prefix, seq, segSuffix), chunk); err != nil {
		w.fail(errors.New("failed to store segment").Base(err))
		w.storage.Put(w.ctx, objectName(w.prefix, seq, errSuffix), nil)
		return
	}
	w.mu.Lock()
	w.pending[seq] = true
	w.mu.Unlock()
}

func (w *walWriter) Close() error {
	w.mu.Lock()
	if w.closed || w.closing {
		w.mu.Unlock()
		return nil
	}
	w.closing = true
	w.notifyLocked()
	for len(w.buf) > 0 && w.err == nil {
		if err := w.waitWindowLocked(false); err != nil {
			w.failLocked(err)
			break
		}
		w.flushLocked()
	}
	w.closed = true
	if w.stall != nil {
		w.stall.Stop()
	}
	w.mu.Unlock()
	defer w.cancel()

	w.wg.Wait()

	w.mu.Lock()
	err, seq := w.err, w.seq
	w.mu.Unlock()

	if err != nil {
		return err
	}
	return w.storage.Put(w.ctx, objectName(w.prefix, seq, endSuffix), nil)
}

type walReader struct {
	ctx     context.Context
	storage Storage
	prefix  string
	params
	seq int64

	ch        chan []byte
	discards  chan string
	wake      chan struct{}
	holeSince time.Time

	errMu   sync.Mutex
	err     error
	workers sync.WaitGroup
}

func newWALReader(ctx context.Context, storage Storage, prefix string, p params) *walReader {
	r := &walReader{
		ctx:      ctx,
		storage:  storage,
		prefix:   prefix,
		params:   p,
		ch:       make(chan []byte, p.concurrency),
		discards: make(chan string, 4*p.concurrency),
		wake:     make(chan struct{}, 1),
	}
	r.workers.Add(2)
	go func() { defer r.workers.Done(); r.run() }()
	go func() { defer r.workers.Done(); r.discardLoop() }()
	return r
}

func (r *walReader) wait() { r.workers.Wait() }

func (r *walReader) chunks() <-chan []byte {
	return r.ch
}

func (r *walReader) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *walReader) run() {
	defer close(r.ch)

	delay := r.minPollInterval
	active := time.Now()
	for {
		polled := time.Now()
		advanced, eof, err := r.poll()
		if err != nil {
			r.setErr(err)
			return
		}
		if eof {
			return
		}

		switch {
		case advanced:
			active = time.Now()
			delay = r.minPollInterval
		case time.Since(active) < r.eagerWindow:
			delay = r.minPollInterval
		default:
			delay *= 2
			if delay > r.maxPollInterval {
				delay = r.maxPollInterval
			}
		}

		timer := time.NewTimer(delay)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-r.wake:
			timer.Stop()
			active = time.Now()
			delay = r.minPollInterval
			if rest := r.minPollInterval - time.Since(polled); rest > 0 {
				select {
				case <-r.ctx.Done():
					return
				case <-time.After(rest):
				}
			}
		}
	}
}

func (r *walReader) poll() (advanced, eof bool, err error) {
	listed, err := r.storage.List(r.ctx, r.prefix)
	if err != nil {
		return false, false, err
	}
	if len(listed) == 0 {
		return false, false, nil
	}

	pending := make(map[int64]Entry, len(listed))
	ahead := false
	for _, entry := range listed {
		seq, ok := parseEntry(entry.Name)
		if !ok {
			continue
		}
		pending[seq] = entry
		if seq > r.seq {
			ahead = true
		}
	}

	if _, ok := pending[r.seq]; !ok && ahead {
		if r.holeSince.IsZero() {
			r.holeSince = time.Now()
		} else if time.Since(r.holeSince) >= r.holeTimeout {
			return false, false, errors.New("segment ", r.seq,
				" never arrived while later ones did, the peer lost it")
		}
	} else {
		r.holeSince = time.Time{}
	}

	for {
		if entry, ok := pending[r.seq]; ok && strings.HasSuffix(entry.Name, errSuffix) {
			r.discard(r.prefix + "/" + entry.Name)
			return advanced, false, errors.New("the peer could not store segment ", r.seq)
		}

		batch, done := r.nextBatch(pending)
		if done {
			return advanced, true, nil
		}
		if len(batch) == 0 {
			return advanced, false, nil
		}

		chunks, err := r.fetch(batch)
		if err != nil {
			if err == errNotFound {
				return advanced, false, nil
			}
			return advanced, false, err
		}

		for i, chunk := range chunks {
			select {
			case r.ch <- chunk:
			case <-r.ctx.Done():
				return advanced, true, nil
			}
			r.seq++
			advanced = true
			r.discard(r.prefix + "/" + batch[i].Name)
		}
	}
}

func (r *walReader) nextBatch(pending map[int64]Entry) (batch []Entry, done bool) {
	for i := 0; i < r.concurrency; i++ {
		entry, ok := pending[r.seq+int64(i)]
		if !ok {
			break
		}
		if !strings.HasSuffix(entry.Name, segSuffix) {
			if i == 0 && strings.HasSuffix(entry.Name, endSuffix) {
				r.discard(r.prefix + "/" + entry.Name)
				return nil, true
			}
			break
		}
		batch = append(batch, entry)
	}
	return batch, false
}

func (r *walReader) fetch(batch []Entry) ([][]byte, error) {
	chunks := make([][]byte, len(batch))
	failures := make([]error, len(batch))

	var wg sync.WaitGroup
	for i, entry := range batch {
		if entry.Inline != nil {
			chunks[i] = entry.Inline
			continue
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			chunks[i], failures[i] = r.storage.Get(r.ctx, r.prefix+"/"+name)
		}(i, entry.Name)
	}
	wg.Wait()

	for i := range batch {
		if failures[i] != nil {
			return nil, failures[i]
		}
	}
	return chunks, nil
}

func (r *walReader) discard(name string) {
	select {
	case r.discards <- name:
	case <-r.ctx.Done():
	}
}

func (r *walReader) discardLoop() {
	var wg sync.WaitGroup
	defer wg.Wait()

	sem := make(chan struct{}, r.concurrency)
	for {
		select {
		case <-r.ctx.Done():
			return
		case name := <-r.discards:
			select {
			case sem <- struct{}{}:
			case <-r.ctx.Done():
				return
			}
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				defer func() { <-sem }()
				for {
					if err := r.storage.Delete(r.ctx, name); err == nil {
						return
					}
					select {
					case <-r.ctx.Done():
						return
					case <-time.After(r.maxPollInterval):
					}
				}
			}(name)
		}
	}
}

func (r *walReader) setErr(err error) {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

func (r *walReader) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if r.err != nil {
		return r.err
	}
	return io.EOF
}
