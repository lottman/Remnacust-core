package core

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
)

func TestCachedNativeHostKeysRemainProtectedWithoutExternalPolicy(t *testing.T) {
	config := &Config{Inbound: []*InboundHandlerConfig{
		{Tag: "protected", ProxySettings: serial.ToTypedMessage(&protocol.User{Email: "123~0123456789abcdef01234567~h11111111111141118111111111111111"})},
		{Tag: "ordinary", ProxySettings: serial.ToTypedMessage(&protocol.User{Email: "ordinary@example.test"})},
	}}
	instance := &Instance{ctx: context.Background()}
	instance.RecordConfiguration(config)
	scope := NewFeatureScope(instance)
	if !instance.RequiresHostPolicy("protected") || !scope.Instance.RequiresHostPolicy("protected") {
		t.Fatal("native host key lost its policy requirement")
	}
	if instance.RequiresHostPolicy("ordinary") {
		t.Fatal("unrelated listener was made protected")
	}
	config.Inbound = config.Inbound[1:]
	instance.RecordConfiguration(config)
	if scope.Instance.RequiresHostPolicy("protected") {
		t.Fatal("removed host scope remained required")
	}
}
