package xerahttp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	. "github.com/xtls/xray-core/transport/internet/xerahttp"
)

func Test_GetNormalizedPath(t *testing.T) {
	tests := []struct {
		TestName           string
		Path               string
		SessionIDPlacement string
		SeqPlacement       string
		Expected           string
	}{
		{
			TestName: "default placement keeps trailing slash",
			Path:     "/sh",
			Expected: "/sh/",
		},
		{
			TestName: "query string is stripped",
			Path:     "/?world",
			Expected: "/",
		},
		{
			TestName:           "both off path drops trailing slash",
			Path:               "/stream",
			SessionIDPlacement: "query",
			SeqPlacement:       "query",
			Expected:           "/stream",
		},
		{
			TestName:           "both off path keeps file-like path",
			Path:               "/stream/filename.extension",
			SessionIDPlacement: "query",
			SeqPlacement:       "header",
			Expected:           "/stream/filename.extension",
		},
		{
			TestName:           "seq in path keeps trailing slash",
			Path:               "/stream",
			SessionIDPlacement: "query",
			Expected:           "/stream/",
		},
		{
			TestName:     "session in path keeps trailing slash",
			Path:         "/stream",
			SeqPlacement: "cookie",
			Expected:     "/stream/",
		},
		{
			TestName:           "existing trailing slash preserved",
			Path:               "/stream/",
			SessionIDPlacement: "query",
			SeqPlacement:       "query",
			Expected:           "/stream/",
		},
		{
			TestName:           "root unchanged",
			Path:               "/",
			SessionIDPlacement: "query",
			SeqPlacement:       "query",
			Expected:           "/",
		},
	}
	for _, test := range tests {
		t.Run(test.TestName, func(t *testing.T) {
			c := Config{
				Path:               test.Path,
				SessionIDPlacement: test.SessionIDPlacement,
				SeqPlacement:       test.SeqPlacement,
			}
			assert.Equal(t, test.Expected, c.GetNormalizedPath())
		})
	}
}

func Test_FillPacketRequest_GetBody(t *testing.T) {
	data := []byte("hello xray")
	payload := buf.MergeBytes(nil, data)

	req, err := http.NewRequest("POST", "https://example.com/", nil)
	common.Must(err)

	config := &Config{}
	config.FillPacketRequest(req, "sess", "0", payload)

	if req.GetBody == nil {
		t.Fatalf("Expected GetBody to be set")
	}

	first, err := io.ReadAll(req.Body)
	common.Must(err)

	if string(data) != string(first) {
		t.Fatalf("Body mismatch. Format %q and %q are not equal", data, first)
	}

	body2, err := req.GetBody()
	common.Must(err)

	second, err := io.ReadAll(body2)
	common.Must(err)

	if string(data) != string(second) {
		t.Fatalf("Replayed body mismatch. Format %q and %q are not equal", data, second)
	}
}

func TestGenerateSessionIDDefaultAvoidsUUIDPattern(t *testing.T) {
	config := &Config{}
	seen := make(map[string]struct{})
	for i := 0; i < 128; i++ {
		id := config.GenerateSessionID()
		if len(id) != 24 {
			t.Fatalf("unexpected default session ID length: %d", len(id))
		}
		if strings.Contains(id, "-") {
			t.Fatalf("default session ID contains UUID separator: %q", id)
		}
		for _, ch := range id {
			if !strings.ContainsRune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", ch) {
				t.Fatalf("default session ID contains unexpected character: %q", id)
			}
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("default session ID repeated: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestDefaultPaddingNormalizationIsShared(t *testing.T) {
	config := &Config{}
	if config.GetNormalizedXPaddingPlacement() != "query" {
		t.Fatal("unexpected default padding placement")
	}
	if config.GetNormalizedXPaddingKey() != "p" {
		t.Fatal("unexpected default padding key")
	}
	if config.GetNormalizedXPaddingHeader() != "X-Pad" {
		t.Fatal("unexpected default padding header")
	}
	if config.GetNormalizedXPaddingMethod() != PaddingMethodTokenish {
		t.Fatal("unexpected default padding method")
	}
}

func TestGenerateSessionIDSupportsMaximumAlphabet(t *testing.T) {
	config := &Config{
		SessionIDTable:  strings.Repeat("a", 256),
		SessionIDLength: &RangeConfig{From: 16, To: 16},
	}
	if id := config.GenerateSessionID(); len(id) != 16 {
		t.Fatalf("unexpected session ID length: %d", len(id))
	}
}

func TestDefaultWireProfileOmitsLegacyMarkers(t *testing.T) {
	config := &Config{}
	request := httptest.NewRequest("POST", "https://example.com/api/v3/socket.js/", strings.NewReader("payload"))
	config.FillStreamRequest(request, "session", "")
	for _, name := range []string{"Referer", "X-Padding", "X-Accel-Buffering"} {
		if request.Header.Get(name) != "" {
			t.Fatalf("default request emitted legacy header %s", name)
		}
	}
	if request.Header.Get("Content-Type") != "" {
		t.Fatal("default request emitted gRPC content type")
	}
	if request.URL.Query().Get("p") == "" {
		t.Fatal("default request omitted XERA padding")
	}
}
