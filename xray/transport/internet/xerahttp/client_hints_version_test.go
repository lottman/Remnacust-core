package xerahttp

import (
	"net/http"
	"testing"
)

func TestClientHintGreaseFollowsPinnedTLSVersion(t *testing.T) {
	for _, profile := range []string{"chrome", "edge"} {
		t.Run(profile, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("User-Agent", "Mozilla/5.0 Chrome/154.0.0.0 Edg/154.0.0.0")
			headers.Set("Sec-CH-UA", `"Chromium";v="154", "Not A(Brand";v="99", "Google Chrome";v="154"`)
			alignHTTPVersion(headers, nil, profile, "155")
			brand := "Google Chrome"
			if profile == "edge" {
				brand = "Microsoft Edge"
			}
			want := `"` + brand + `";v="155", "Chromium";v="155", "Not(A:Brand";v="24"`
			if got := headers.Get("Sec-CH-UA"); got != want {
				t.Fatalf("client hints retained another version's GREASE profile: %q, want %q", got, want)
			}
		})
	}
}

func TestClientHintVersionPreservesExplicitValue(t *testing.T) {
	headers := make(http.Header)
	const hints = `"Private Browser";v="1"`
	headers.Set("User-Agent", "Chrome/154.0.0.0")
	headers.Set("Sec-CH-UA", hints)
	alignHTTPVersion(headers, map[string]string{"sec-ch-ua": hints}, "chrome", "155")
	if got := headers.Get("Sec-CH-UA"); got != hints {
		t.Fatalf("explicit client hints changed: %q", got)
	}
}
