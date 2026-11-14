package xerahttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
)

func packetReservationHandler(t *testing.T) *requestHandler {
	t.Helper()
	h := testAdmissionHandler(t, "POST")
	h.ln.handler = h
	t.Cleanup(func() { h.ln.Close() })
	return h
}

func packetReservationClient(t *testing.T, protocol string, handler http.Handler) (*http.Client, string) {
	t.Helper()
	if protocol != "h3" {
		server := httptest.NewUnstartedServer(handler)
		if protocol == "h2" {
			server.EnableHTTP2 = true
			server.StartTLS()
		} else {
			server.Start()
		}
		t.Cleanup(server.Close)
		client := server.Client()
		client.Timeout = 3 * time.Second
		return client, server.URL
	}
	certificate, _ := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
	pem, key := certificate.ToPEM()
	pair, err := tls.X509KeyPair(pem, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	listener, err := stdnet.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http3.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, NextProtos: []string{"h3"}}}
	served := make(chan struct{})
	go func() { defer close(served); server.Serve(listener) }()
	t.Cleanup(func() {
		server.Close()
		listener.Close()
		select {
		case <-served:
		case <-time.After(2 * time.Second):
			t.Error("HTTP/3 server did not stop")
		}
	})
	transport := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}}
	t.Cleanup(func() { transport.Close() })
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}, "https://" + listener.LocalAddr().String()
}

func TestPacketReservationSmallNativeUploadWithLargeMaximum(t *testing.T) {
	for _, protocol := range []string{"http1", "h2", "h3"} {
		t.Run(protocol, func(t *testing.T) {
			h := packetReservationHandler(t)
			h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
			returned := make(chan struct{})
			client, origin := packetReservationClient(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(w, r)
				close(returned)
			}))
			request, err := http.NewRequest("POST", origin+"/api/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = "example.com"
			if err := h.config.FillPacketRequest(request, "small", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			<-returned
			wantProtocol := map[string]int{"http1": 1, "h2": 2, "h3": 3}[protocol]
			if response.ProtoMajor != wantProtocol || response.StatusCode != http.StatusOK {
				t.Fatalf("one-byte upload returned %s status %d", response.Proto, response.StatusCode)
			}
			if used := h.budget.used.Load(); used < 1 || used > h.budget.limit || h.pendingRequests.Load() != 0 {
				t.Fatalf("invalid queued accounting: %d", used)
			}
			payload := make([]byte, 1)
			if _, err := io.ReadFull(h.upsertSession("small").uploadQueue, payload); err != nil || string(payload) != "x" || h.budget.used.Load() != 0 {
				t.Fatalf("queued payload was lost or retained: %q %v", payload, err)
			}
			h.ln.Close()
		})
	}
}

type packetReservationGateBody struct {
	io.ReadCloser
	once    sync.Once
	entered chan<- struct{}
	release <-chan struct{}
}

func (b *packetReservationGateBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		b.entered <- struct{}{}
		<-b.release
	})
	return b.ReadCloser.Read(p)
}

func TestPacketReservationConcurrentSmallKnownBodies(t *testing.T) {
	h := packetReservationHandler(t)
	const count = 16
	entered, gate := make(chan struct{}, count), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	results := make(chan int, count)
	var workers sync.WaitGroup
	defer func() { release(); workers.Wait() }()
	for i := range count {
		request := httptest.NewRequest("POST", "https://example.com/api/", nil)
		if err := h.config.FillPacketRequest(request, fmt.Sprintf("small%d", i), "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
			t.Fatal(err)
		}
		request.Body = &packetReservationGateBody{ReadCloser: request.Body, entered: entered, release: gate}
		workers.Go(func() {
			response := newPacketDeadlineRecorder()
			h.ServeHTTP(response, request)
			results <- response.Code
		})
	}
	for range count {
		select {
		case <-entered:
		case status := <-results:
			t.Fatalf("small known body rejected before reading: %d", status)
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent uploads did not enter their body readers")
		}
	}
	if used := h.budget.used.Load(); used <= 0 || used >= 4<<20 || h.pendingRequests.Load() != count {
		t.Fatalf("small pending bodies over-reserved: budget=%d pending=%d", used, h.pendingRequests.Load())
	}
	release()
	workers.Wait()
	for range count {
		if status := <-results; status != http.StatusOK {
			t.Fatalf("small upload returned %d", status)
		}
	}
	if used := h.budget.used.Load(); used < count || used > h.budget.limit || h.pendingRequests.Load() != 0 {
		t.Fatalf("invalid queued accounting: %d", used)
	}
	h.ln.Close()
	if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 || h.sessionCount.Load() != 0 {
		t.Fatal("listener cleanup retained packet reservations")
	}
}

func TestPacketReservationUnknownLengthKeepsWorstCase(t *testing.T) {
	for _, length := range []int64{-2, -1, 0} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			h := packetReservationHandler(t)
			h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
			r := httptest.NewRequest("POST", "https://example.com/api/", nil)
			if err := h.config.FillPacketRequest(r, "unknown", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
				t.Fatal(err)
			}
			body := &observedUploadBody{ReadCloser: r.Body}
			r.Body, r.ContentLength = body, length
			response := newPacketDeadlineRecorder()
			h.ServeHTTP(response, r)
			if response.Code != http.StatusServiceUnavailable || body.reads != 0 {
				t.Fatalf("unknown body escaped worst-case admission: status=%d reads=%d", response.Code, body.reads)
			}
			if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 || h.sessionCount.Load() != 0 {
				t.Fatal("rejected unknown body retained admission")
			}
		})
	}
}

func TestPacketReservationHeaderAndCookieHaveNoBodyReservation(t *testing.T) {
	for _, placement := range []string{PlacementHeader, PlacementCookie} {
		t.Run(placement, func(t *testing.T) {
			h := packetReservationHandler(t)
			h.config.UplinkDataPlacement, h.config.UplinkDataKey = placement, "X-Data"
			h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
			h.budget.limit = 128 << 10
			r := httptest.NewRequest("POST", "https://example.com/api/", nil)
			if err := h.config.FillPacketRequest(r, "metadata", "0", buf.MergeBytes(nil, []byte("payload"))); err != nil {
				t.Fatal(err)
			}
			body := &observedUploadBody{ReadCloser: io.NopCloser(strings.NewReader("unread"))}
			r.Body, r.ContentLength = body, 1<<60
			response := newPacketDeadlineRecorder()
			h.ServeHTTP(response, r)
			if response.Code != http.StatusOK || body.reads != 0 || h.budget.used.Load() < 7 {
				t.Fatalf("metadata payload rejected or body read: status=%d reads=%d", response.Code, body.reads)
			}
			payload := make([]byte, 7)
			if _, err := io.ReadFull(h.upsertSession("metadata").uploadQueue, payload); err != nil || string(payload) != "payload" || h.budget.used.Load() != 0 {
				t.Fatalf("metadata payload lost or retained: %q %v", payload, err)
			}
		})
	}
}

func TestPacketReservationOversizedKnownLengthPrecedesAdmission(t *testing.T) {
	for _, padded := range []bool{false, true} {
		t.Run(fmt.Sprint(padded), func(t *testing.T) {
			h := packetReservationHandler(t)
			h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
			h.budget.limit = 1
			if padded {
				h.config.CustomDownlinkPadding = testDownlinkConfig()
				h.config.CustomDownlinkPadding.Uplink = true
			}
			r := httptest.NewRequest("POST", "https://example.com/api/", nil)
			if err := h.config.FillPacketRequest(r, "oversized", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
				t.Fatal(err)
			}
			body := &observedUploadBody{ReadCloser: r.Body}
			r.Body, r.ContentLength = body, (16<<20)+1
			if padded {
				r.Header.Set(h.config.CustomDownlinkPadding.Header, h.config.CustomDownlinkPadding.wireValue())
				r.ContentLength = (16<<20)*4 + 65537
			}
			response := newPacketDeadlineRecorder()
			h.ServeHTTP(response, r)
			if response.Code != http.StatusRequestEntityTooLarge || body.reads != 0 {
				t.Fatalf("oversized known body was not rejected before admission: status=%d reads=%d", response.Code, body.reads)
			}
			if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 || h.sessionCount.Load() != 0 {
				t.Fatal("oversized request retained admission")
			}
		})
	}
}

type packetReservationCountingBody struct {
	io.ReadCloser
	read int
}

func (b *packetReservationCountingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += n
	return n, err
}

func TestPacketReservationPaddedKnownWireLengthBoundsReads(t *testing.T) {
	for _, scenario := range []string{"valid", "short-source", "extra-source", "truncated-frame", "malformed-frame", "oversized-decoded"} {
		t.Run(scenario, func(t *testing.T) {
			h := packetReservationHandler(t)
			h.config.CustomDownlinkPadding = testDownlinkConfig()
			h.config.CustomDownlinkPadding.Uplink = true
			h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
			h.budget.limit = 128 << 10
			r := httptest.NewRequest("POST", "https://example.com/api/", nil)
			if err := h.config.FillPacketRequest(r, "padded", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
				t.Fatal(err)
			}
			r.Header.Set(h.config.CustomDownlinkPadding.Header, h.config.CustomDownlinkPadding.wireValue())
			wire := []byte{0, 1, 0, 0, 'x'}
			r.ContentLength = int64(len(wire))
			want := http.StatusOK
			switch scenario {
			case "short-source":
				wire = wire[:4]
				want = http.StatusNotFound
			case "extra-source":
				wire = append(wire, bytes.Repeat([]byte("ignored"), 10000)...)
			case "truncated-frame":
				r.ContentLength = 4
				want = http.StatusNotFound
			case "malformed-frame":
				wire[1] = 0
				want = http.StatusNotFound
			case "oversized-decoded":
				h.config.ScMaxEachPostBytes = &RangeConfig{From: 1, To: 1}
				wire, r.ContentLength = []byte{0, 2, 0, 0, 'x', 'y'}, 6
				want = http.StatusRequestEntityTooLarge
			}
			body := &packetReservationCountingBody{ReadCloser: io.NopCloser(bytes.NewReader(wire))}
			r.Body = body
			wireLength := r.ContentLength
			response := newPacketDeadlineRecorder()
			h.ServeHTTP(response, r)
			if response.Code != want || int64(body.read) > wireLength {
				t.Fatalf("padded body bounds changed: status=%d want=%d read=%d", response.Code, want, body.read)
			}
			if want == http.StatusOK {
				payload := make([]byte, 1)
				if _, err := io.ReadFull(h.upsertSession("padded").uploadQueue, payload); err != nil || string(payload) != "x" {
					t.Fatalf("padded payload lost: %q %v", payload, err)
				}
			}
			if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
				t.Fatal("padded request retained its reservation")
			}
		})
	}
}

func TestPacketReservationCancelledKnownBodyReleasesAdmission(t *testing.T) {
	h := packetReservationHandler(t)
	h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, writer, result := stalledPacket(t, h, "cancelled", ctx)
	defer writer.Close()
	if used := h.budget.used.Load(); used <= 0 || used > 128<<10 {
		t.Fatalf("small stalled request reservation = %d", used)
	}
	cancel()
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled small upload did not stop")
	}
	if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 || h.sessionCount.Load() != 0 {
		t.Fatal("cancelled known body retained admission")
	}
}

func TestPacketReservationAggregateBudgetStillRejectsBeforeRead(t *testing.T) {
	h := packetReservationHandler(t)
	h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
	h.budget.limit = 128 << 10
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, writer, result := stalledPacket(t, h, "first", ctx)
	defer writer.Close()
	reserved := h.budget.used.Load()
	r := httptest.NewRequest("POST", "https://example.com/api/", nil)
	if err := h.config.FillPacketRequest(r, "second", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	body := &observedUploadBody{ReadCloser: r.Body}
	r.Body = body
	response := newPacketDeadlineRecorder()
	h.ServeHTTP(response, r)
	if response.Code != http.StatusServiceUnavailable || body.reads != 0 || h.budget.used.Load() != reserved || reserved > h.budget.limit {
		t.Fatal("concurrent small body bypassed aggregate admission")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("first upload did not stop")
	}
	if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 || h.sessionCount.Load() != 0 {
		t.Fatal("aggregate admission retained request resources")
	}
}

func TestPacketReservationKnownPayloadSurvivesQueueAndDuplicate(t *testing.T) {
	h := packetReservationHandler(t)
	h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
	payload := bytes.Repeat([]byte("x"), 64<<10)
	var reserved int64
	for range 2 {
		r := httptest.NewRequest("POST", "https://example.com/api/", nil)
		if err := h.config.FillPacketRequest(r, "queued", "0", buf.MergeBytes(nil, payload)); err != nil {
			t.Fatal(err)
		}
		response := newPacketDeadlineRecorder()
		h.ServeHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Fatalf("queued payload returned %d", response.Code)
		}
		used := h.budget.used.Load()
		if used < int64(len(payload)) || used > h.budget.limit || reserved != 0 && used != reserved {
			t.Fatalf("queue or duplicate accounting = %d, previous %d", used, reserved)
		}
		reserved = used
	}
	q := h.upsertSession("queued").uploadQueue
	got := make([]byte, len(payload)/2)
	if _, err := io.ReadFull(q, got); err != nil || !bytes.Equal(got, payload[:len(got)]) || h.budget.used.Load() != reserved {
		t.Fatal("partial queue read lost payload ownership")
	}
	q.Close()
	if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
		t.Fatal("queue close retained known payload")
	}
}

func TestPacketReservationAutoPlacementAccountsCombinedPayload(t *testing.T) {
	h := packetReservationHandler(t)
	h.config.UplinkDataPlacement, h.config.UplinkDataKey = PlacementAuto, "X-Data"
	h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
	r := httptest.NewRequest("POST", "https://example.com/api/", nil)
	if err := h.config.FillPacketRequest(r, "combined", "0", buf.MergeBytes(nil, []byte("body"))); err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Data-0", base64.RawURLEncoding.EncodeToString([]byte("header")))
	r.AddCookie(&http.Cookie{Name: "X-Data_0", Value: base64.RawURLEncoding.EncodeToString([]byte("cookie"))})
	response := newPacketDeadlineRecorder()
	h.ServeHTTP(response, r)
	if response.Code != http.StatusOK {
		t.Fatalf("combined upload returned %d", response.Code)
	}
	q := h.upsertSession("combined").uploadQueue
	q.mu.Lock()
	if len(q.heap) != 1 {
		t.Error("combined packet was not queued")
	} else if packet := q.heap[0]; packet.lease == nil || packet.lease.bytes.Load() < int64(cap(packet.Payload)) {
		t.Error("combined payload was under-accounted")
	}
	q.mu.Unlock()
	payload := make([]byte, len("headercookiebody"))
	if _, err := io.ReadFull(q, payload); err != nil || string(payload) != "headercookiebody" || h.budget.used.Load() != 0 {
		t.Fatalf("combined payload lost or retained: %q %v", payload, err)
	}
}

func TestPacketReservationPaddedNativeUpload(t *testing.T) {
	for _, protocol := range []string{"http1", "h2", "h3"} {
		t.Run(protocol, func(t *testing.T) {
			h := packetReservationHandler(t)
			h.config.ScMaxEachPostBytes = &RangeConfig{From: 16 << 20, To: 16 << 20}
			h.config.CustomDownlinkPadding = testDownlinkConfig()
			h.config.CustomDownlinkPadding.Uplink = true
			returned := make(chan struct{})
			client, origin := packetReservationClient(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(w, r)
				close(returned)
			}))
			r, err := http.NewRequest("POST", origin+"/api/", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Host = "example.com"
			if err := h.config.FillPacketRequest(r, "padded", "0", buf.MergeBytes(nil, []byte("x"))); err != nil {
				t.Fatal(err)
			}
			r.Header.Set(h.config.CustomDownlinkPadding.Header, h.config.CustomDownlinkPadding.wireValue())
			r.Body, r.ContentLength = planPaddedPacket(r.Body, h.config.CustomDownlinkPadding, nil, r.ContentLength)
			r.GetBody = nil
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			<-returned
			if response.StatusCode != http.StatusOK || h.budget.used.Load() < 1 {
				t.Fatalf("padded native upload returned %d", response.StatusCode)
			}
			payload := make([]byte, 1)
			if _, err := io.ReadFull(h.upsertSession("padded").uploadQueue, payload); err != nil || string(payload) != "x" || h.budget.used.Load() != 0 {
				t.Fatalf("native padded payload lost or retained: %q %v", payload, err)
			}
		})
	}
}

func TestPacketReservationNativeContentLengthMismatch(t *testing.T) {
	for _, protocol := range []string{"http1", "h2", "h3"} {
		for _, length := range []int64{1, 3} {
			t.Run(fmt.Sprintf("%s/%d", protocol, length), func(t *testing.T) {
				h := packetReservationHandler(t)
				entered := make(chan struct{})
				returned := make(chan struct{})
				client, origin := packetReservationClient(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.Body = &packetHTTPBody{ReadCloser: r.Body, entered: entered}
					h.ServeHTTP(w, r)
					close(returned)
				}))
				r, err := http.NewRequest("POST", origin+"/api/", nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Host = "example.com"
				if err := h.config.FillPacketRequest(r, "mismatch", "0", buf.MergeBytes(nil, []byte("xy"))); err != nil {
					t.Fatal(err)
				}
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				r.Body, r.ContentLength, r.GetBody = reader, length, nil
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					response, _ := client.Do(r)
					if response != nil {
						response.Body.Close()
					}
				}()
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("native mismatched body did not reach the handler")
				}
				writer.Write([]byte("xy"))
				writer.Close()
				select {
				case <-returned:
				case <-time.After(2 * time.Second):
					t.Fatal("native mismatched body did not finish")
				}
				<-finished
				if stored, ok := h.sessions.Load("mismatch"); ok {
					q := stored.(*httpSession).uploadQueue
					q.mu.Lock()
					for _, packet := range q.heap {
						if length > 2 || len(packet.Payload) > int(length) || packet.lease == nil || packet.lease.bytes.Load() < int64(cap(packet.Payload)) {
							t.Error("mismatched upload was truncated or under-accounted")
						}
					}
					q.mu.Unlock()
				}
				h.ln.Close()
				if h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 || h.sessionCount.Load() != 0 {
					t.Fatal("native mismatched body retained admission")
				}
			})
		}
	}
}
