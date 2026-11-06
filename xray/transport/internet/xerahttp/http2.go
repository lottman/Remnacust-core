package xerahttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func (c *Config) http2Config() (*http.HTTP2Config, error) {
	if len(c.H2Params) == 0 {
		return nil, nil
	}
	if len(c.H2Params) > 4096 || bytes.Equal(bytes.TrimSpace(c.H2Params), []byte("null")) {
		return nil, fmt.Errorf("h2Params must be a JSON object of at most 4096 bytes")
	}
	config := new(http.HTTP2Config)
	decoder := json.NewDecoder(bytes.NewReader(c.H2Params))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(config); err != nil {
		return nil, fmt.Errorf("invalid h2Params: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("h2Params must contain exactly one JSON object")
	}
	if config.PermitProhibitedCipherSuites || config.StrictMaxConcurrentRequests {
		return nil, fmt.Errorf("h2Params cannot enable prohibited ciphers or transport-only options")
	}
	if config.MaxConcurrentStreams < 0 || config.MaxConcurrentStreams > 4096 {
		return nil, fmt.Errorf("h2Params.MaxConcurrentStreams must be within 0..4096")
	}
	for _, size := range []int{config.MaxDecoderHeaderTableSize, config.MaxEncoderHeaderTableSize, config.MaxReceiveBufferPerStream} {
		if size < 0 || size >= 4<<20 {
			return nil, fmt.Errorf("h2Params table and stream buffers must be smaller than 4 MiB")
		}
	}
	if size := config.MaxReceiveBufferPerConnection; size != 0 && (size < 65536 || size >= 4<<20) {
		return nil, fmt.Errorf("h2Params connection buffer must be within 65536..4194303")
	}
	if size := config.MaxReadFrameSize; size != 0 && (size < 16384 || size > 16777215) {
		return nil, fmt.Errorf("h2Params.MaxReadFrameSize must be within 16384..16777215")
	}
	for _, timeout := range []time.Duration{config.SendPingTimeout, config.PingTimeout, config.WriteByteTimeout} {
		if timeout < 0 || timeout > 24*time.Hour {
			return nil, fmt.Errorf("h2Params timeouts must be within 0..24 hours in nanoseconds")
		}
	}
	return config, nil
}
