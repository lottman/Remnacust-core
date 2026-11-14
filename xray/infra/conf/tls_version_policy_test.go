package conf

import "testing"

func TestTLSVersionPolicyBuildValidation(t *testing.T) {
	for _, scenario := range []struct {
		minimum string
		maximum string
		valid   bool
	}{
		{"", "", true}, {"1.2", "", true}, {"", "1.3", true},
		{"1.2", "1.3", true}, {"1.3", "1.3", true}, {"1.0", "1.1", true},
		{"1.4", "", false}, {"", "TLS1.3", false}, {"1.3", "1.2", false}, {"1.00", "1.3", false},
	} {
		t.Run(scenario.minimum+"/"+scenario.maximum, func(t *testing.T) {
			_, err := (&TLSConfig{MinVersion: scenario.minimum, MaxVersion: scenario.maximum}).Build()
			if (err == nil) != scenario.valid {
				t.Fatalf("TLS version policy validation differs: %v", err)
			}
		})
	}
}
