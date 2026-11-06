package xerahttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet/stat"
)

func packetBodySynctest(t *testing.T, test func(*testing.T)) {
	log.Record(&log.GeneralMessage{Severity: log.Severity_Debug, Content: "packet body virtual clock fixture"})
	synctest.Test(t, test)
}

type packetDeadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (w *packetDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()
	return nil
}

func newPacketDeadlineRecorder() *packetDeadlineRecorder {
	return &packetDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (w *packetDeadlineRecorder) EnableFullDuplex() error { return nil }

type signaledPacketBody struct {
	*io.PipeReader
	entered chan struct{}
	once    sync.Once
}

func (b *signaledPacketBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	return b.PipeReader.Read(p)
}

func stalledPacket(t *testing.T, h *requestHandler, id string, ctx context.Context) (*signaledPacketBody, *io.PipeWriter, <-chan int) {
	t.Helper()
	r := httptest.NewRequest("POST", "https://example.com/api/", nil).WithContext(ctx)
	if err := h.config.FillPacketRequest(r, id, "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	body := &signaledPacketBody{PipeReader: reader, entered: make(chan struct{})}
	r.Body, r.ContentLength = body, 1
	result := make(chan int, 1)
	go func() {
		response := newPacketDeadlineRecorder()
		h.ServeHTTP(response, r)
		result <- response.Code
	}()
	<-body.entered
	return body, writer, result
}

func TestPacketBodiesReleaseBudgetAfterSessionExpiry(t *testing.T) {
	packetBodySynctest(t, func(t *testing.T) {
		h := testAdmissionHandler(t, "POST")
		reservation := int64(h.config.GetNormalizedScMaxEachPostBytes().To)*4 + int64(h.config.GetNormalizedServerMaxHeaderBytes())*4 + 65536
		n := int(h.budget.limit / reservation)
		writers := make([]*io.PipeWriter, n)
		results := make([]<-chan int, n)
		for i := range n {
			_, writers[i], results[i] = stalledPacket(t, h, fmt.Sprintf("stalled%d", i), context.Background())
		}
		time.Sleep(31 * time.Second)
		synctest.Wait()
		retained, pending := h.budget.used.Load(), h.pendingRequests.Load()
		for i := range n {
			writers[i].Close()
			<-results[i]
		}
		if retained != 0 || pending != 0 || h.sessionCount.Load() != 0 {
			t.Fatalf("expired bodies retained budget=%d pending=%d", retained, pending)
		}
		r := httptest.NewRequest("POST", "https://example.com/api/", nil)
		if err := h.config.FillPacketRequest(r, "legitimate", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
			t.Fatal(err)
		}
		response := newPacketDeadlineRecorder()
		h.ServeHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Fatalf("legitimate upload returned %d", response.Code)
		}
	})
}

func TestConnectedPacketBodyHasFiniteReadTime(t *testing.T) {
	packetBodySynctest(t, func(t *testing.T) {
		h := testAdmissionHandler(t, "POST")
		h.upsertSession("connected").isFullyConnected.Close()
		_, writer, result := stalledPacket(t, h, "connected", context.Background())
		time.Sleep(net.ConnIdleTimeout + time.Second)
		synctest.Wait()
		retained, pending := h.budget.used.Load(), h.pendingRequests.Load()
		writer.Close()
		status := <-result
		if retained != 0 || pending != 0 || status == http.StatusOK {
			t.Fatalf("body read deadline did not release admission: budget=%d pending=%d status=%d", retained, pending, status)
		}
	})
}

func TestPacketBodyCancellationReleasesAdmission(t *testing.T) {
	for _, cause := range []string{"request", "session", "listener"} {
		t.Run(cause, func(t *testing.T) {
			packetBodySynctest(t, func(t *testing.T) {
				h := testAdmissionHandler(t, "POST")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				_, writer, result := stalledPacket(t, h, "cancelled", ctx)
				switch cause {
				case "request":
					cancel()
				case "session":
					h.upsertSession("cancelled").uploadQueue.Close()
				case "listener":
					h.stopped.Close()
				}
				synctest.Wait()
				retained, pending := h.budget.used.Load(), h.pendingRequests.Load()
				writer.Close()
				<-result
				if retained != 0 || pending != 0 {
					t.Fatalf("%s retained budget=%d pending=%d", cause, retained, pending)
				}
			})
		})
	}
}

func TestSlowPacketBodyWithinReadBoundIsAccepted(t *testing.T) {
	packetBodySynctest(t, func(t *testing.T) {
		h := testAdmissionHandler(t, "POST")
		session := h.upsertSession("slow")
		session.isFullyConnected.Close()
		r := httptest.NewRequest("POST", "https://example.com/api/", nil)
		if err := h.config.FillPacketRequest(r, "slow", "0", buf.MergeBytes(nil, []byte("abcdef"))); err != nil {
			t.Fatal(err)
		}
		reader, writer := io.Pipe()
		r.Body, r.ContentLength = reader, 6
		go func() {
			defer writer.Close()
			for _, value := range []byte("abcdef") {
				time.Sleep(40 * time.Second)
				if _, err := writer.Write([]byte{value}); err != nil {
					return
				}
			}
		}()
		response := newPacketDeadlineRecorder()
		h.ServeHTTP(response, r)
		if response.Code != http.StatusOK || h.pendingRequests.Load() != 0 {
			t.Fatalf("slow valid upload rejected: %d", response.Code)
		}
		if len(response.deadlines) != 2 || !response.deadlines[1].IsZero() {
			t.Fatal("successful packet retained its read deadline")
		}
		payload := make([]byte, 6)
		if _, err := io.ReadFull(session.uploadQueue, payload); err != nil || string(payload) != "abcdef" || h.budget.used.Load() != 0 {
			t.Fatalf("slow upload was truncated or retained: %q %v", payload, err)
		}
	})
}

func TestPacketGuardRejectsUnsupportedDeadlineBeforeReadingBody(t *testing.T) {
	h := testAdmissionHandler(t, "POST")
	r := httptest.NewRequest("POST", "https://example.com/api/", nil)
	if err := h.config.FillPacketRequest(r, "unsupported", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	body := &observedUploadBody{ReadCloser: r.Body}
	r.Body = body
	response := httptest.NewRecorder()
	h.ServeHTTP(response, r)
	if response.Code != http.StatusNotFound || body.reads != 0 || h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
		t.Fatal("unsupported deadline consumed body data or retained admission")
	}
}

func TestPacketQueueBackpressureOutlivesBodyDeadline(t *testing.T) {
	packetBodySynctest(t, func(t *testing.T) {
		h := testAdmissionHandler(t, "POST")
		h.config.ScMaxBufferedPosts = 1
		session := h.upsertSession("queued")
		session.isFullyConnected.Close()
		if err := session.uploadQueue.Push(Packet{Payload: []byte("ahead"), Seq: 2}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "https://example.com/api/", nil)
		if err := h.config.FillPacketRequest(r, "queued", "3", buf.MergeBytes(nil, []byte("x"))); err != nil {
			t.Fatal(err)
		}
		response := newPacketDeadlineRecorder()
		returned := make(chan struct{})
		go func() { defer close(returned); h.ServeHTTP(response, r) }()
		synctest.Wait()
		if len(response.deadlines) != 2 || !response.deadlines[1].IsZero() {
			t.Fatal("body deadline was not cleared before waiting for queue space")
		}
		time.Sleep(2 * net.ConnIdleTimeout)
		synctest.Wait()
		select {
		case <-returned:
			t.Fatal("body deadline interrupted queue backpressure")
		default:
		}
		session.uploadQueue.Close()
		<-returned
		if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
			t.Fatal("closed queue retained pending admission")
		}
	})
}

func TestPacketGuardCompletionCancellationRaceDoesNotLeaveCallbacks(t *testing.T) {
	packetBodySynctest(t, func(t *testing.T) {
		for range 200 {
			ctx, cancel := context.WithCancel(context.Background())
			r := httptest.NewRequest("POST", "https://example.com/api/", nil).WithContext(ctx)
			q := NewUploadQueue(1)
			stopped := make(chan struct{})
			response := newPacketDeadlineRecorder()
			finish, err := guardPacketBody(response, r, r.Body, &httpSession{uploadQueue: q}, stopped)
			if err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			workers.Go(func() { finish(true) })
			workers.Go(cancel)
			workers.Go(func() { q.Close() })
			workers.Wait()
			count := len(response.deadlines)
			close(stopped)
			time.Sleep(net.ConnIdleTimeout + time.Second)
			synctest.Wait()
			if len(response.deadlines) != count {
				t.Fatal("deadline callback survived packet cleanup")
			}
		}
	})
}

func TestStreamingAndLongPollingDoNotAcquirePacketReadDeadlines(t *testing.T) {
	for _, mode := range []string{"download", ModeStreamUp, ModeStreamOne} {
		t.Run(mode, func(t *testing.T) {
			packetBodySynctest(t, func(t *testing.T) {
				h := testAdmissionHandler(t, "POST")
				h.config.Mode = ModeAuto
				connected := make(chan stat.Connection, 1)
				h.ln.addConn = func(c stat.Connection) { connected <- c }
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				method, id := "POST", "stream"
				if mode == "download" {
					method = "GET"
				} else if mode == ModeStreamOne {
					id = ""
				} else {
					h.upsertSession(id).isFullyConnected.Close()
				}
				r := httptest.NewRequest(method, "https://example.com/api/", nil).WithContext(ctx)
				h.config.FillStreamRequest(r, id, "")
				response := newPacketDeadlineRecorder()
				returned := make(chan struct{})
				go func() { defer close(returned); h.ServeHTTP(response, r) }()
				if mode != ModeStreamUp {
					<-connected
				}
				time.Sleep(2 * net.ConnIdleTimeout)
				synctest.Wait()
				select {
				case <-returned:
					t.Fatal("stream or long poll was limited by packet body lifetime")
				default:
				}
				cancel()
				<-returned
				synctest.Wait()
				if len(response.deadlines) != 0 || response.Code != http.StatusOK {
					t.Fatal("packet body policy affected streaming")
				}
			})
		})
	}
}
