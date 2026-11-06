package xerahttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/signal/done"
)

func testAdmissionHandler(t *testing.T, method string) *requestHandler {
	t.Helper()
	config := &Config{Host: "example.com", Path: "/api/", Mode: ModePacketUp, UplinkHTTPMethod: method}
	ln := &Listener{config: config}
	h := &requestHandler{config: config, ln: ln, host: config.Host, path: config.GetNormalizedPath(), sessionMu: &sync.Mutex{}, stopped: done.New()}
	h.budget.limit = config.serverMemoryBudget()
	t.Cleanup(func() {
		h.stopped.Close()
		h.sessions.Range(func(key, value any) bool {
			value.(*httpSession).uploadQueue.Close()
			return true
		})
	})
	return h
}

func TestUnrecognizedRequestsShareResponseAndAllocateNothing(t *testing.T) {
	h := testAdmissionHandler(t, "POST")
	var expectedHeader http.Header
	var expectedBody string
	for _, scenario := range []string{"host", "path", "method", "padding", "padding-alphabet", "session", "sequence", "sequence-method", "sequence-session", "mode", "options", "preflight-method", "preflight-origin"} {
		t.Run(scenario, func(t *testing.T) {
			request := httptest.NewRequest("POST", "https://example.com/api/", nil)
			if err := h.config.FillPacketRequest(request, "session", "0", buf.MergeBytes(nil, []byte("unread"))); err != nil {
				t.Fatal(err)
			}
			body := &observedUploadBody{ReadCloser: request.Body}
			request.Body = body
			switch scenario {
			case "host":
				request.Host = "other.example"
			case "path":
				request.URL.Path = "/other/"
			case "method":
				request.Method = "DELETE"
			case "padding":
				request.URL.RawQuery = ""
			case "padding-alphabet":
				request.URL.RawQuery = "p=" + strings.Repeat("!", 200)
			case "session":
				request.URL.Path = "/api/" + strings.Repeat("a", maxSessionIDLength+1) + "/0"
			case "sequence":
				request.URL.Path = "/api/session/not-a-sequence"
			case "sequence-method":
				request.Method = "GET"
			case "sequence-session":
				request.URL.Path = "/api//0"
			case "mode":
				request.URL.Path = "/api/session"
			case "options":
				request.Method = "OPTIONS"
			case "preflight-method":
				request.Method = "OPTIONS"
				request.Header.Set("Origin", "https://browser.example")
				request.Header.Set("Access-Control-Request-Method", "DELETE")
			case "preflight-origin":
				request.Method = "OPTIONS"
				request.Header.Set("Access-Control-Request-Method", "POST")
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound || body.reads != 0 || h.sessionCount.Load() != 0 || h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
				t.Fatal("unrecognized request was distinguishable or retained resources")
			}
			if expectedHeader == nil {
				expectedHeader, expectedBody = response.Header().Clone(), response.Body.String()
			} else if !reflect.DeepEqual(expectedHeader, response.Header()) || expectedBody != response.Body.String() {
				t.Fatal("unrecognized responses differ")
			}
		})
	}
}

func TestCORSPreflightDoesNotCreateSession(t *testing.T) {
	h := testAdmissionHandler(t, "POST")
	request := httptest.NewRequest("OPTIONS", "https://example.com/api/", nil)
	request.Header.Set("Origin", "https://browser.example")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "X-Pad, X-Session")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Access-Control-Allow-Origin") != "https://browser.example" || response.Header().Get("Access-Control-Allow-Methods") != "POST" || h.sessionCount.Load() != 0 {
		t.Fatal("valid browser preflight failed")
	}
	if response.Header().Get("X-Pad") != "" || response.Body.Len() != 0 {
		t.Fatal("preflight exposed transport padding")
	}
}

func TestCustomPaddingCannotSilentlyDowngrade(t *testing.T) {
	for _, uplink := range []bool{false, true} {
		t.Run(strconv.FormatBool(uplink), func(t *testing.T) {
			h := testAdmissionHandler(t, "POST")
			h.config.CustomDownlinkPadding = testDownlinkConfig()
			h.config.CustomDownlinkPadding.Uplink = uplink
			custom := h.config.CustomDownlinkPadding
			var expectedHeader http.Header
			var expectedBody string
			for _, scenario := range []string{"path", "missing", "wrong-token", "wrong-version", "duplicate", "preflight"} {
				t.Run(scenario, func(t *testing.T) {
					request := httptest.NewRequest("POST", "https://example.com/api/", nil)
					if err := h.config.FillPacketRequest(request, "session", "0", buf.MergeBytes(nil, []byte("unread"))); err != nil {
						t.Fatal(err)
					}
					body := &observedUploadBody{ReadCloser: request.Body}
					request.Body = body
					request.Header.Set(custom.Header, custom.wireValue())
					switch scenario {
					case "path":
						request.URL.Path = "/other/"
					case "missing":
						request.Header.Del(custom.Header)
					case "wrong-token":
						request.Header.Set(custom.Header, "1.another_random_token_123456")
					case "wrong-version":
						request.Header.Set(custom.Header, "3."+custom.Token)
					case "duplicate":
						request.Header.Add(custom.Header, custom.wireValue())
					case "preflight":
						request.Method = "OPTIONS"
						request.Header.Set("Origin", "https://browser.example")
						request.Header.Set("Access-Control-Request-Method", "POST")
					}
					response := httptest.NewRecorder()
					h.ServeHTTP(response, request)
					if response.Code != http.StatusNotFound || body.reads != 0 || h.sessionCount.Load() != 0 || h.budget.used.Load() != 0 || h.pendingRequests.Load() != 0 {
						t.Fatal("invalid marker consumed transport resources or accepted a downgrade")
					}
					if expectedHeader == nil {
						expectedHeader, expectedBody = response.Header().Clone(), response.Body.String()
					} else if !reflect.DeepEqual(expectedHeader, response.Header()) || expectedBody != response.Body.String() {
						t.Fatal("marker failure differs from ordinary not-found response")
					}
				})
			}
		})
	}
}

func TestOPTIONSUplinkReceivesPayloadAndResponsePadding(t *testing.T) {
	h := testAdmissionHandler(t, "OPTIONS")
	request := httptest.NewRequest("OPTIONS", "https://example.com/api/", nil)
	if err := h.config.FillPacketRequest(request, "session", "0", buf.MergeBytes(nil, []byte("payload"))); err != nil {
		t.Fatal(err)
	}
	response := newPacketDeadlineRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !h.config.IsPaddingValid(response.Header().Get("X-Pad"), 100, 1000, PaddingMethodTokenish) {
		t.Fatal("OPTIONS upload or response padding failed")
	}
	stored, ok := h.sessions.Load("session")
	if !ok {
		t.Fatal("OPTIONS was treated as a preflight")
	}
	got, err := io.ReadAll(io.LimitReader(stored.(*httpSession).uploadQueue, 7))
	if err != nil || string(got) != "payload" {
		t.Fatal("upload did not reach the session", err)
	}
}
