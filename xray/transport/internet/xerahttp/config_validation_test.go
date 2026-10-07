package xerahttp

import "testing"

func TestValidateRejectsInvalidXmuxRanges(t *testing.T) {
	tests := []struct {
		name string
		set  func(*XmuxConfig)
	}{
		{name: "negative connections", set: func(x *XmuxConfig) { x.MaxConnections = &RangeConfig{From: -1, To: 2} }},
		{name: "inverted concurrency", set: func(x *XmuxConfig) { x.MaxConcurrency = &RangeConfig{From: 4, To: 2} }},
		{name: "oversized requests", set: func(x *XmuxConfig) { x.HMaxRequestTimes = &RangeConfig{From: 1, To: maxXMUXRequests + 1} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &Config{Xmux: &XmuxConfig{}}
			test.set(config.Xmux)
			if err := config.Validate(); err == nil {
				t.Fatal("invalid XMUX range was accepted")
			}
		})
	}
}

func TestValidateAcceptsStreamAuto(t *testing.T) {
	if err := (&Config{Mode: ModeStreamAuto}).Validate(); err != nil {
		t.Fatalf("stream-auto was rejected: %v", err)
	}
}

func TestValidateRejectsPacketOnlyOptionsInStreamAuto(t *testing.T) {
	if err := (&Config{Mode: ModeStreamAuto, UplinkDataPlacement: PlacementHeader}).Validate(); err == nil {
		t.Fatal("stream-auto accepted header payload placement")
	}
	if err := (&Config{Mode: ModeStreamAuto, UplinkHTTPMethod: "GET"}).Validate(); err == nil {
		t.Fatal("stream-auto accepted GET uplink")
	}
}
