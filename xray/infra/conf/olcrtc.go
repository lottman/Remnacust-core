package conf

import "github.com/xtls/xray-core/proxy/olcrtc"

type OlcrtcConfig struct {
	Provider  string `json:"provider"`
	Transport string `json:"transport"`
	RoomID    string `json:"roomId"`
	CryptoKey string `json:"cryptoKey"`
	AuthToken string `json:"authToken"`
	DeviceID  string `json:"deviceId"`
	DNSServer string `json:"dnsServer"`
}

func (c *OlcrtcConfig) Build() (*olcrtc.Config, error) {
	config := &olcrtc.Config{
		Provider: c.Provider, Transport: c.Transport, RoomId: c.RoomID,
		CryptoKey: c.CryptoKey, AuthToken: c.AuthToken, DeviceId: c.DeviceID,
		DnsServer: c.DNSServer,
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}
