package xerahttp

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestDefaultPaddingUsesConfiguredMethod(t *testing.T) {
	config := &Config{XPaddingBytes: &RangeConfig{From: 128, To: 128}}
	for range 12 {
		request := httptest.NewRequest("POST", "https://example.com/api/", nil)
		config.FillStreamRequest(request, "session", "")
		value, _ := config.ExtractXPaddingFromRequest(request)
		if value == strings.Repeat("X", len(value)) || !config.IsPaddingValid(value, 128, 128, PaddingMethodTokenish) {
			t.Fatal("default request did not contain valid tokenish padding")
		}
	}
	if GeneratePadding("repeat-x", 128) != "" || config.IsPaddingValid(strings.Repeat("!", 128), 128, 128, PaddingMethodTokenish) {
		t.Fatal("legacy method or invalid padding alphabet was accepted")
	}
}

func TestHTTPHeadersFollowTLSFamilyWithoutMutatingConfig(t *testing.T) {
	for _, family := range []string{"firefox", "safari", "chrome"} {
		t.Run(family, func(t *testing.T) {
			original := &Config{Headers: map[string]string{"Accept-Language": "fa", "Sec-Fetch-Site": "cross-site"}}
			settings := &internet.MemoryStreamConfig{ProtocolSettings: original, SecurityType: "tls", SecuritySettings: &tls.Config{Fingerprint: family}}
			config := configForHTTPClient(settings)
			headers := config.GetRequestHeader()
			ua := strings.ToLower(headers.Get("User-Agent"))
			if !strings.Contains(ua, family) || headers.Get("Accept-Language") != "fa" || headers.Get("Sec-Fetch-Site") != "cross-site" {
				t.Fatal("HTTP family or explicit header overrides were lost")
			}
			if family == "firefox" && headers.Get("Sec-CH-UA") != "" {
				t.Fatal("Firefox request contains Chromium client hints")
			}
			if len(original.Headers) != 2 || config.GetRequestHeader().Get("User-Agent") != headers.Get("User-Agent") {
				t.Fatal("request profile mutated the source config or changed between requests")
			}
		})
	}
}

func TestHTTPHeadersPreserveExplicitUserAgent(t *testing.T) {
	config := configForHTTPClient(&internet.MemoryStreamConfig{ProtocolSettings: &Config{Headers: map[string]string{"user-agent": "Example/1.0"}}, SecurityType: "tls", SecuritySettings: &tls.Config{Fingerprint: "firefox"}})
	if config.GetRequestHeader().Get("User-Agent") != "Example/1.0" {
		t.Fatal("explicit user agent was replaced")
	}
	config = configForHTTPClient(&internet.MemoryStreamConfig{ProtocolSettings: &Config{Headers: map[string]string{"User-Agent": "golang"}}})
	if config.GetRequestHeader().Get("User-Agent") != "" || config.GetRequestHeader().Get("Sec-CH-UA") != "" {
		t.Fatal("native HTTP headers were replaced by browser defaults")
	}
}

func TestHTTPVersionMatchesPinnedTLSProfile(t *testing.T) {
	config := configForHTTPClient(&internet.MemoryStreamConfig{ProtocolSettings: &Config{}, SecurityType: "tls", SecuritySettings: &tls.Config{Fingerprint: "hellochrome_120"}})
	headers := config.GetRequestHeader()
	if !strings.Contains(headers.Get("User-Agent"), "Chrome/120.") || !strings.Contains(headers.Get("Sec-CH-UA"), `"Chromium";v="120"`) {
		t.Fatal("HTTP browser version does not match pinned TLS profile")
	}
	if headers.Get("DNT") != "1" {
		t.Fatal("noncanonical browser header was lost")
	}
}

func TestMetadataRejectsHeaderAndPlacementCollisions(t *testing.T) {
	for _, config := range []*Config{
		{XPaddingHeader: "Content-Length"},
		{XPaddingHeader: "X-Forwarded-For"},
		{XPaddingHeader: "X-Bad\r\nHeader"},
		{SessionIDPlacement: PlacementHeader, SessionIDKey: "X-Shared", SeqPlacement: PlacementHeader, SeqKey: "x-shared"},
		{SessionIDPlacement: PlacementQuery, SessionIDKey: "p"},
		{XPaddingPlacement: PlacementPath},
		{SessionIDPlacement: PlacementQueryInHeader},
		{CustomDownlinkPadding: &DownlinkPaddingConfig{Header: "X-Pad", Token: "0123456789abcdef"}},
	} {
		if err := config.Validate(); err == nil {
			t.Fatal("unsafe metadata configuration was accepted")
		}
	}
}
