package conf

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVlessInboundRejectsVisionWithXera(t *testing.T) {
	var config InboundDetourConfig
	input := `{"protocol":"vless","port":443,"settings":{"clients":[],"decryption":"none","flow":"xtls-rprx-vision"},"streamSettings":{"network":"xera","xeraHttpSettings":{}}}`
	if err := json.Unmarshal([]byte(input), &config); err != nil {
		t.Fatal(err)
	}
	_, err := config.Build()
	if err == nil || !strings.Contains(err.Error(), "incompatible with XERA-HTTP") {
		t.Fatalf("expected XERA flow validation error, got %v", err)
	}
}

func TestVlessOutboundRejectsVisionWithXera(t *testing.T) {
	var config OutboundDetourConfig
	input := `{"protocol":"vless","settings":{"address":"127.0.0.1","port":443,"id":"00000000-0000-0000-0000-000000000001","flow":"xtls-rprx-vision","encryption":"none"},"streamSettings":{"network":"xera","xeraHttpSettings":{}}}`
	if err := json.Unmarshal([]byte(input), &config); err != nil {
		t.Fatal(err)
	}
	_, err := config.Build()
	if err == nil || !strings.Contains(err.Error(), "incompatible with XERA-HTTP") {
		t.Fatalf("expected XERA flow validation error, got %v", err)
	}
}
