package tls

import (
	stdtls "crypto/tls"
	"testing"
)

func TestUTLSConfigPreservesRecordSizing(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		config := copyConfig(&stdtls.Config{DynamicRecordSizingDisabled: disabled})
		if config.DynamicRecordSizingDisabled != disabled {
			t.Fatalf("record sizing policy lost: got %v, want %v", config.DynamicRecordSizingDisabled, disabled)
		}
	}
}
