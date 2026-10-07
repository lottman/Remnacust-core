package xerahttp

import "testing"

func TestResolveStreamAutoMode(t *testing.T) {
	tests := []struct {
		name                string
		httpVersion         string
		isReality           bool
		hasDownloadSettings bool
		expected            string
	}{
		{name: "reality h2", httpVersion: "2", isReality: true, expected: ModeStreamOne},
		{name: "reality h2 with downlink", httpVersion: "2", isReality: true, hasDownloadSettings: true, expected: ModeStreamUp},
		{name: "tls h2 behind possible CDN", httpVersion: "2", expected: ModePacketUp},
		{name: "tls h3", httpVersion: "3", expected: ModePacketUp},
		{name: "plain h1", httpVersion: "1.1", expected: ModePacketUp},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode := resolveMode(ModeStreamAuto, test.httpVersion, test.isReality, test.hasDownloadSettings)
			if mode != test.expected {
				t.Fatalf("mode = %q, want %q", mode, test.expected)
			}
		})
	}
}

func TestResolveLegacyModeKeepsCompatibility(t *testing.T) {
	if got := resolveMode(ModeAuto, "2", false, false); got != ModePacketUp {
		t.Fatalf("plain auto mode = %q, want %q", got, ModePacketUp)
	}
	if got := resolveMode(ModeAuto, "2", true, false); got != ModeStreamOne {
		t.Fatalf("reality auto mode = %q, want %q", got, ModeStreamOne)
	}
	if got := resolveMode(ModeAuto, "2", true, true); got != ModeStreamUp {
		t.Fatalf("reality auto downlink mode = %q, want %q", got, ModeStreamUp)
	}
}
