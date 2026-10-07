package olcrtc

import (
	"encoding/hex"
	"fmt"
	"strings"
)

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("olcRTC config is missing")
	}
	if strings.TrimSpace(c.Provider) == "" {
		return fmt.Errorf("olcRTC provider is required")
	}
	switch c.Provider {
	case "jitsi", "telemost", "wbstream", "none":
	default:
		return fmt.Errorf("olcRTC provider %q is unsupported", c.Provider)
	}
	switch c.Transport {
	case "datachannel", "videochannel", "seichannel", "vp8channel":
	default:
		return fmt.Errorf("olcRTC transport %q is unsupported", c.Transport)
	}
	if strings.TrimSpace(c.RoomId) == "" && c.Provider != "none" {
		return fmt.Errorf("olcRTC roomId is required")
	}
	key, err := hex.DecodeString(c.CryptoKey)
	if err != nil || len(key) != 32 {
		return fmt.Errorf("olcRTC cryptoKey must be 32 bytes of hexadecimal")
	}
	if strings.TrimSpace(c.DnsServer) == "" {
		return fmt.Errorf("olcRTC dnsServer is required")
	}
	return nil
}
