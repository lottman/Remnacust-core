package xerahttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/signal/done"
)

type observedUploadBody struct {
	io.ReadCloser
	reads int
}

func (r *observedUploadBody) Read(data []byte) (int, error) {
	r.reads++
	return r.ReadCloser.Read(data)
}

func TestUploadAdmissionPrecedesBodyRead(t *testing.T) {
	for _, limit := range []int64{1, 1 << 20} {
		config := &Config{Path: "/resource", Mode: "packet-up", ScMaxEachPostBytes: &RangeConfig{From: 1024, To: 1024}}
		ln := &Listener{config: config}
		h := &requestHandler{config: config, ln: ln, path: config.GetNormalizedPath(), sessionMu: &sync.Mutex{}, stopped: done.New()}
		h.budget.limit = limit
		ln.handler = h
		request := httptest.NewRequest("POST", "http://localhost/resource/", nil)
		if err := config.FillPacketRequest(request, "session", "0", buf.MergeBytes(nil, []byte("payload"))); err != nil {
			t.Fatal(err)
		}
		body := &observedUploadBody{ReadCloser: request.Body}
		request.Body = body
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if limit == 1 {
			if response.Code != http.StatusServiceUnavailable || body.reads != 0 || h.budget.used.Load() != 0 {
				t.Fatal("rejected upload was read or retained")
			}
		} else if response.Code != http.StatusOK || body.reads == 0 || h.budget.used.Load() == 0 {
			t.Fatal("queued upload was not accounted")
		}
		ln.Close()
		if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
			t.Fatal("listener retained upload resources")
		}
	}
}

func TestPayloadBudgetConcurrentLimit(t *testing.T) {
	b := &payloadBudget{limit: 1024}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 100 {
				if lease := b.acquire(128); lease != nil {
					if used := b.used.Load(); used < 0 || used > b.limit {
						t.Errorf("budget exceeded: %d", used)
					}
					lease.release()
					lease.release()
				}
			}
		})
	}
	workers.Wait()
	if b.used.Load() != 0 || b.acquire(1025) != nil || b.acquire(-1) != nil {
		t.Fatal("invalid budget accounting")
	}
}

func TestQueueReleasesPayloadReservations(t *testing.T) {
	b := &payloadBudget{limit: 100}
	q := NewUploadQueue(1)
	defer q.Close()
	push := func(seq uint64) {
		t.Helper()
		lease := b.acquire(20)
		if lease == nil {
			t.Fatal("reservation unavailable")
		}
		if err := q.Push(Packet{Seq: seq, Payload: []byte("ab"), lease: lease}); err != nil {
			t.Fatal(err)
		}
	}
	push(1)
	push(1)
	if b.used.Load() != 20 {
		t.Fatal("duplicate retained reservation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	lease := b.acquire(20)
	go func() { result <- q.PushContext(ctx, Packet{Seq: 2, Payload: []byte("x"), lease: lease}) }()
	cancel()
	select {
	case err := <-result:
		if err == nil || b.used.Load() != 20 {
			t.Fatal("canceled request retained reservation")
		}
	case <-time.After(time.Second):
		t.Fatal("push did not stop")
	}
	push(0)
	if _, err := io.ReadFull(q, make([]byte, 4)); err != nil || b.used.Load() != 0 {
		t.Fatal("consumed packets retained reservations")
	}
	push(3)
	q.Close()
	if b.used.Load() != 0 {
		t.Fatal("closed queue retained reservation")
	}
}

func TestSessionLimitAndListenerCleanup(t *testing.T) {
	config := &Config{ServerMaxSessions: 2}
	ln := &Listener{config: config}
	h := &requestHandler{config: config, ln: ln, sessionMu: &sync.Mutex{}, stopped: done.New()}
	ln.handler = h
	a, b := h.upsertSession("a"), h.upsertSession("b")
	if a == nil || b == nil || h.upsertSession("c") != nil || h.upsertSession("a") != a {
		t.Fatal("session limit not enforced")
	}
	if !h.deleteSession("a", a) || h.upsertSession("c") == nil {
		t.Fatal("session slot not released")
	}
	a.isFullyConnected.Close()
	a.uploadQueue.Close()
	ln.Close()
	if h.sessionCount.Load() != 0 || h.upsertSession("late") != nil || !b.uploadQueue.closed {
		t.Fatal("listener retained sessions")
	}
}

func TestListenerCloseCancelsActiveHTTP(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	ln := &Listener{}
	ln.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	server := httptest.NewUnstartedServer(ln.server.Handler)
	server.Config = &ln.server
	server.Start()
	defer server.Close()
	ln.listener = server.Listener
	requestDone := make(chan struct{})
	go func() {
		resp, _ := server.Client().Get(server.URL)
		if resp != nil {
			resp.Body.Close()
		}
		close(requestDone)
	}()
	<-started
	ln.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("active HTTP handler survived close")
	}
	<-requestDone
}
