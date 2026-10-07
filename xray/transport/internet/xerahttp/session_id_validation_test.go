package xerahttp

import "testing"

func TestSessionIDValidationRejectsWeakOrAmbiguousSettings(t *testing.T) {
	for _, config := range []*Config{
		{SessionIDLength: &RangeConfig{From: 1, To: 1}},
		{SessionIDTable: "aaaaaaaaaaaaaaaa", SessionIDLength: &RangeConfig{From: 32, To: 32}},
		{SessionIDTable: "hex", SessionIDLength: &RangeConfig{From: 8, To: 64}},
		{SessionIDTable: "abc/def", SessionIDLength: &RangeConfig{From: 64, To: 64}},
		{SessionIDTable: "abc\rdef", SessionIDLength: &RangeConfig{From: 64, To: 64}},
	} {
		if err := config.Validate(); err == nil {
			t.Errorf("unsafe session ID settings accepted: table=%q length=%v", config.SessionIDTable, config.SessionIDLength)
		}
	}
}

func TestSessionIDValidationAcceptsStrongSettings(t *testing.T) {
	for _, config := range []*Config{
		{},
		{SessionIDTable: "hex", SessionIDLength: &RangeConfig{From: 32, To: 64}},
		{SessionIDTable: "Base62", SessionIDLength: &RangeConfig{From: 22, To: 24}},
	} {
		if err := config.Validate(); err != nil {
			t.Errorf("strong settings rejected: %v", err)
		}
	}
}
