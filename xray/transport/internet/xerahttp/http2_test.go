package xerahttp

import "testing"

func TestHTTP2ParametersValidateBounds(t *testing.T) {
	for _, value := range []string{
		`null`, `[]`, `{} {}`, `{"unknown":1}`, `{"MaxReadFrameSize":1}`,
		`{"MaxReadFrameSize":16777216}`, `{"MaxConcurrentStreams":4097}`,
		`{"MaxDecoderHeaderTableSize":4194304}`, `{"MaxReceiveBufferPerConnection":1}`,
		`{"SendPingTimeout":-1}`, `{"PingTimeout":86400000000001}`,
		`{"PermitProhibitedCipherSuites":true}`, `{"StrictMaxConcurrentRequests":true}`,
	} {
		config := &Config{H2Params: []byte(value)}
		if err := config.Validate(); err == nil {
			t.Errorf("accepted invalid HTTP/2 parameters: %s", value)
		}
	}
	config := &Config{H2Params: []byte(`{"MaxConcurrentStreams":128,"MaxReadFrameSize":65536,"MaxReceiveBufferPerConnection":1048576,"SendPingTimeout":30000000000}`)}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	parsed, err := config.http2Config()
	if err != nil || parsed.MaxConcurrentStreams != 128 || parsed.MaxReadFrameSize != 65536 {
		t.Fatal("valid parameters did not reach HTTP/2 configuration", err)
	}
}
