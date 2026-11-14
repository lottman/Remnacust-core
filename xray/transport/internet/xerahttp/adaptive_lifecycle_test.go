package xerahttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

func adaptiveNativeClient(t *testing.T, controller *adaptiveController, roundTrip func(*http.Request) (*http.Response, error)) *DefaultDialerClient {
	t.Helper()
	raw, peer := net.Pipe()
	t.Cleanup(func() { raw.Close(); peer.Close() })
	return &DefaultDialerClient{adaptive: controller, transportConfig: &Config{}, client: &http.Client{Transport: leaseRoundTrip(func(request *http.Request) (*http.Response, error) {
		if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.GotConn != nil {
			trace.GotConn(httptrace.GotConnInfo{Conn: raw})
		}
		return roundTrip(request)
	})}}
}

func TestAdaptiveNativeOpenStreamLeases(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   bool
		upload bool
		active [2]int
	}{{"download", false, false, [2]int{1, 0}}, {"duplex", true, false, [2]int{1, 1}}, {"upload", true, true, [2]int{0, 1}}} {
		t.Run(test.name, func(t *testing.T) {
			controller, clock := adaptiveTestController(AdaptiveBalanced, 1, 1)
			respond := make(chan struct{})
			var requests atomic.Int32
			client := adaptiveNativeClient(t, controller, func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				select {
				case <-respond:
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
				return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("rejected"))}, nil
			})
			var body io.Reader
			if test.body {
				body = strings.NewReader("payload")
			}
			reader, _, _, err := client.OpenStream(context.Background(), "https://example.test/", "session", body, test.upload)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reader.Close() })
			_, active, _ := adaptiveSnapshot(controller)
			if active != test.active {
				t.Fatalf("request count %v, want %v", active, test.active)
			}
			clock.advance(time.Second)
			controller.mu.Lock()
			for _, direction := range controller.directions {
				if direction.baseline != 0 || direction.window.bytes != 0 {
					t.Error("GotConn recorded successful HTTP feedback")
				}
			}
			controller.mu.Unlock()
			close(respond)
			if _, err := io.ReadAll(reader); err == nil {
				t.Fatal("HTTP rejection lost after GotConn")
			}
			reader.Close()
			leaseEventually(t, func() bool { _, active, _ := adaptiveSnapshot(controller); return active == [2]int{} })
			if requests.Load() != 1 {
				t.Fatal("rejected request was replayed")
			}
		})
	}
}

func TestAdaptiveNativeBodyEOFAndCloseReleaseOnce(t *testing.T) {
	controller, _ := adaptiveTestController(AdaptiveBalanced, 1, 1)
	client := adaptiveNativeClient(t, controller, func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("payload"))}, nil
	})
	reader, _, _, err := client.OpenStream(context.Background(), "https://example.test/", "session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "payload" {
		t.Fatalf("body %q: %v", data, err)
	}
	reader.Close()
	reader.Close()
	_, active, _ := adaptiveSnapshot(controller)
	if active != [2]int{} {
		t.Fatal(active)
	}
	controller.mu.Lock()
	progress := controller.directions[0].window.bytes
	controller.mu.Unlock()
	if progress != 7 {
		t.Fatalf("payload counted %d times", progress)
	}
}

func TestAdaptiveNativeCancellationBeforeResponseDrainsLease(t *testing.T) {
	controller, _ := adaptiveTestController(AdaptiveBalanced, 1, 1)
	finished := make(chan struct{})
	client := adaptiveNativeClient(t, controller, func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		close(finished)
		return nil, request.Context().Err()
	})
	reader, _, _, err := client.OpenStream(context.Background(), "https://example.test/", "session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, _, err := client.OpenStream(ctx, "https://example.test/", "other", nil, false)
		result <- err
	}()
	leaseEventually(t, func() bool { _, _, queued := adaptiveSnapshot(controller); return queued == 1 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reader.Close()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("request cancellation did not finish")
	}
	leaseEventually(t, func() bool {
		_, active, queued := adaptiveSnapshot(controller)
		return active == [2]int{} && queued == 0
	})
}

func TestAdaptivePostPacketCancellationReleasesPayloadAndDirections(t *testing.T) {
	controller, _ := adaptiveTestController(AdaptiveBalanced, 1, 1)
	posted := make(chan struct{})
	complete := make(chan struct{})
	client := adaptiveNativeClient(t, controller, func(request *http.Request) (*http.Response, error) {
		if request.Body != nil {
			io.Copy(io.Discard, request.Body)
			request.Body.Close()
			close(posted)
			<-complete
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	result := make(chan error, 1)
	go func() {
		result <- client.PostPacket(context.Background(), "https://example.test/", "session", "0", buf.MergeBytes(nil, []byte("first")))
	}()
	<-posted
	down, _, _, err := client.OpenStream(context.Background(), "https://example.test/", "session", nil, false)
	if err != nil {
		t.Fatal("occupied upload blocked download:", err)
	}
	down.Close()
	ctx, cancel := context.WithCancel(context.Background())
	packet := buf.New()
	packet.Write([]byte("pending"))
	queued := make(chan error, 1)
	go func() {
		queued <- client.PostPacket(ctx, "https://example.test/", "session", "1", buf.MultiBuffer{packet})
	}()
	leaseEventually(t, func() bool { _, _, queued := adaptiveSnapshot(controller); return queued == 1 })
	cancel()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !packet.IsEmpty() {
		t.Fatal("canceled pending packet retained its payload")
	}
	close(complete)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	leaseEventually(t, func() bool {
		_, active, queued := adaptiveSnapshot(controller)
		return active == [2]int{} && queued == 0
	})
}

func TestAdaptiveBrowserReadinessDoesNotFabricateResponseLatency(t *testing.T) {
	controller, clock := adaptiveTestController(AdaptiveBalanced, 1, 1)
	lease := adaptiveAcquire(t, controller, adaptiveDown)
	clock.advance(10 * time.Second)
	reader := &adaptiveReader{ReadCloser: io.NopCloser(strings.NewReader("body")), lease: lease, direction: adaptiveDown, release: true, confirmOnProgress: true}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "body" {
		t.Fatalf("browser body: %q %v", data, err)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.directions[0].latency != 0 || controller.directions[0].baseline != 0 || controller.directions[0].window.bytes != 4 || controller.directions[0].active != 0 {
		t.Fatal("browser readiness/body progress was used as a fabricated headers or RTT sample")
	}
}

func TestAdaptiveDuplexResponseEOFKeepsUploadPermit(t *testing.T) {
	controller, _ := adaptiveTestController(AdaptiveBalanced, 1, 1)
	requestBody := make(chan io.ReadCloser, 1)
	client := adaptiveNativeClient(t, controller, func(request *http.Request) (*http.Response, error) {
		requestBody <- request.Body
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	input, writer := io.Pipe()
	reader, _, _, err := client.OpenStream(context.Background(), "https://example.test/", "", input, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close(); writer.Close() })
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	_, active, _ := adaptiveSnapshot(controller)
	if active != [2]int{1, 1} {
		t.Fatalf("response EOF released a still-active upload request: %v", active)
	}
	body := <-requestBody
	finished := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, body); body.Close(); finished <- err }()
	if _, err := writer.Write([]byte("upload-after-response-eof")); err != nil {
		t.Fatal("response EOF aborted upload:", err)
	}
	writer.Close()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	leaseEventually(t, func() bool { _, active, _ := adaptiveSnapshot(controller); return active == [2]int{} })
	reader.Close()
}
