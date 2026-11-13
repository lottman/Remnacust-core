package xerahttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xtls/xray-core/common/buf"
)

func malformedPacket(t *testing.T, h *requestHandler, id string) int {
	t.Helper()
	r := httptest.NewRequest("POST", "https://example.com/api/", nil)
	if err := h.config.FillPacketRequest(r, id, "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	r.Body = io.NopCloser(strings.NewReader(""))
	w := newPacketDeadlineRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestMalformedPacketsCannotExhaustSessionSlots(t *testing.T) {
	h := testAdmissionHandler(t, "POST")
	h.config.ServerMaxSessions = 2
	for i := range 8 {
		if status := malformedPacket(t, h, fmt.Sprintf("bad%d", i)); status != http.StatusNotFound {
			t.Fatalf("malformed packet %d returned %d", i, status)
		}
		if h.sessionCount.Load() != 0 || h.pendingRequests.Load() != 0 || h.budget.used.Load() != 0 {
			t.Fatal("malformed packet retained a session or admission budget")
		}
	}
	r := httptest.NewRequest("POST", "https://example.com/api/", nil)
	if err := h.config.FillPacketRequest(r, "valid", "0", buf.MergeBytes(nil, []byte("valid"))); err != nil {
		t.Fatal(err)
	}
	w := newPacketDeadlineRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || h.sessionCount.Load() != 1 {
		t.Fatal("valid packet lost its session after malformed traffic")
	}
	stored, ok := h.sessions.Load("valid")
	if !ok {
		t.Fatal("valid session missing")
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(stored.(*httpSession).uploadQueue, got); err != nil || string(got) != "valid" {
		t.Fatal("valid payload missing", err)
	}
	if status := malformedPacket(t, h, "valid"); status != http.StatusNotFound {
		t.Fatalf("malformed packet on existing session returned %d", status)
	}
	if current, _ := h.sessions.Load("valid"); current != stored || h.sessionCount.Load() != 1 {
		t.Fatal("malformed request destroyed an accepted session")
	}
}

func TestFailedPacketDoesNotDeleteConcurrentSession(t *testing.T) {
	h := testAdmissionHandler(t, "POST")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, writer, result := stalledPacket(t, h, "shared", ctx)
	defer writer.Close()
	if status := malformedPacket(t, h, "shared"); status != http.StatusNotFound {
		t.Fatalf("malformed packet returned %d", status)
	}
	if _, ok := h.sessions.Load("shared"); !ok || h.sessionCount.Load() != 1 {
		t.Fatal("failed request deleted a session used by another request")
	}
	if _, err := io.Copy(writer, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if status := <-result; status != http.StatusOK {
		t.Fatalf("concurrent valid upload returned %d", status)
	}
	if h.sessionCount.Load() != 1 {
		t.Fatal("successful concurrent upload lost its session")
	}
}

func TestRejectedPacketAdmissionReleasesEveryNewSession(t *testing.T) {
	for _, scenario := range []string{"header", "cookie", "size", "body", "deadline", "budget", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			h := testAdmissionHandler(t, "POST")
			h.config.UplinkDataPlacement = PlacementAuto
			h.config.UplinkDataKey = "X-Data"
			r := httptest.NewRequest("POST", "https://example.com/api/", nil)
			if err := h.config.FillPacketRequest(r, "rejected", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
				t.Fatal(err)
			}
			var w http.ResponseWriter = newPacketDeadlineRecorder()
			switch scenario {
			case "header":
				r.Header.Set("X-Data-0", "!")
			case "cookie":
				r.AddCookie(&http.Cookie{Name: "X-Data_0", Value: "!"})
			case "size":
				r.ContentLength = int64(h.config.GetNormalizedScMaxEachPostBytes().To) + 1
			case "body":
				r.Body = io.NopCloser(strings.NewReader(""))
			case "deadline":
				w = httptest.NewRecorder()
			case "budget":
				h.budget.limit = 1
			case "cancelled":
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			h.ServeHTTP(w, r)
			if h.sessionCount.Load() != 0 || h.pendingRequests.Load() != 0 || h.budget.used.Load() != 0 {
				t.Fatal("rejected request retained session or payload admission")
			}
		})
	}
}

func TestConcurrentMalformedPacketsReleaseSharedSession(t *testing.T) {
	h := testAdmissionHandler(t, "POST")
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			if status := malformedPacket(t, h, "shared"); status != http.StatusNotFound {
				t.Errorf("malformed packet returned %d", status)
			}
		})
	}
	workers.Wait()
	if h.sessionCount.Load() != 0 || h.pendingRequests.Load() != 0 || h.budget.used.Load() != 0 {
		t.Fatal("concurrent failed admission retained session resources")
	}
}
