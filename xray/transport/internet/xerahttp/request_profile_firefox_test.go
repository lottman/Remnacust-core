package xerahttp

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestFirefoxRevisionMatchesPinnedTLSProfile(t *testing.T) {
	for _, headers := range []map[string]string{nil, {"User-Agent": "firefox"}, {"user-agent": "firefox"}} {
		settings := &internet.MemoryStreamConfig{
			ProtocolSettings: &Config{Headers: headers}, SecurityType: "tls",
			SecuritySettings: &tls.Config{Fingerprint: "hellofirefox_148"},
		}
		ua := configForHTTPClient(settings).GetRequestHeader().Get("User-Agent")
		if !strings.Contains(ua, "rv:148.0)") || !strings.Contains(ua, "Firefox/148.0") {
			t.Fatalf("Firefox revision and version differ from TLS profile: %q", ua)
		}
	}
}

func TestFirefoxRevisionAlignmentPreservesExplicitHeaders(t *testing.T) {
	for _, test := range []struct {
		name     string
		explicit map[string]string
		want     string
	}{
		{name: "automatic", want: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0"},
		{name: "family", explicit: map[string]string{"User-Agent": "firefox"}, want: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0"},
		{name: "explicit-client-hints", explicit: map[string]string{"Sec-CH-UA": "custom"}, want: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0"},
		{name: "literal", explicit: map[string]string{"user-agent": "Custom Firefox/154.0 rv:154.0"}, want: "Custom Firefox/154.0 rv:154.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:154.0) Gecko/20100101 Firefox/154.0")
			for name, value := range test.explicit {
				if strings.EqualFold(name, "User-Agent") && value != "firefox" {
					headers.Set(name, value)
				}
				if strings.EqualFold(name, "Sec-CH-UA") {
					headers.Set(name, value)
				}
			}
			alignHTTPVersion(headers, test.explicit, "firefox", "148")
			if got := headers.Get("User-Agent"); got != test.want {
				t.Fatalf("User-Agent = %q, want %q", got, test.want)
			}
			if hints := test.explicit["Sec-CH-UA"]; hints != "" && headers.Get("Sec-CH-UA") != hints {
				t.Fatal("explicit client hints changed")
			}
		})
	}
	const literal = "Custom Firefox/154.0 rv:154.0"
	settings := &internet.MemoryStreamConfig{
		ProtocolSettings: &Config{Headers: map[string]string{"user-agent": literal}}, SecurityType: "tls",
		SecuritySettings: &tls.Config{Fingerprint: "hellofirefox_148"},
	}
	if got := configForHTTPClient(settings).GetRequestHeader().Get("User-Agent"); got != literal {
		t.Fatalf("explicit User-Agent changed: %q", got)
	}
}
