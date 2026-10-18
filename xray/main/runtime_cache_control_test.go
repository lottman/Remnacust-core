package main

import (
	"testing"

	"github.com/xtls/xray-core/app/commander"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
)

func TestRuntimeControlRebasesCurrentAndLegacyAPIWithoutAddingADuplicateListener(t *testing.T) {
	const current = "/run/remnacust-xray-New1234567/api.sock,0600"
	t.Setenv("XTLS_API_SOCKET_PATH", "/run/remnacust-xray-New1234567/api.sock")
	t.Setenv("INTERNAL_SOCKET_PATH", "")
	t.Setenv("INTERNAL_REST_TOKEN", "")
	for _, tag := range []string{"REMNAWAVE_API_INBOUND", "REMNAWAVE_API", "public"} {
		const previous = "/run/remnacust-xray-Old1234567/api.sock,0600"
		config := &core.Config{Inbound: []*core.InboundHandlerConfig{{Tag: tag, ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{Listen: net.NewIPOrDomain(net.DomainAddress(previous))})}}}
		if err := rebaseRuntimeControl(config); err != nil {
			t.Fatal(err)
		}
		value, err := config.Inbound[0].ReceiverSettings.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		expected := current
		if tag == "public" {
			expected = previous
		}
		if got := value.(*proxyman.ReceiverConfig).Listen.AsAddress().Domain(); got != expected {
			t.Fatalf("inbound %s: got %q, want %q", tag, got, expected)
		}
	}
	for _, listen := range []string{"", "/run/remnacust-xray-Old1234567/api.sock,0600"} {
		config := &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(&commander.Config{Tag: "REMNAWAVE_API", Listen: listen})}}
		if err := rebaseRuntimeControl(config); err != nil {
			t.Fatal(err)
		}
		value, err := config.App[0].GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		expected := current
		if listen == "" {
			expected = ""
		}
		if got := value.(*commander.Config).Listen; got != expected {
			t.Fatalf("commander listen %q: got %q, want %q", listen, got, expected)
		}
	}
}

func TestRuntimeControlRebasesPrivateAndLegacyWebhooksOnly(t *testing.T) {
	for _, socket := range []string{"/run/remnacust-xray-New1234567/internal.sock", "legacy-new"} {
		t.Run(socket, func(t *testing.T) {
			t.Setenv("INTERNAL_SOCKET_PATH", socket)
			t.Setenv("INTERNAL_REST_TOKEN", "new-token")
			urls := []string{
				"@legacy-old:/internal/webhook?token=old",
				"/run/remnacust-xray-Old1234567/internal.sock:/internal/webhook?token=old",
				"https://example.invalid/internal/webhook?token=external",
				"/tmp/custom.sock:/internal/webhook?token=external",
			}
			routing := &router.Config{}
			for _, url := range urls {
				routing.Rule = append(routing.Rule, &router.RoutingRule{Webhook: &router.WebhookConfig{Url: url}})
			}
			config := &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(routing)}}
			if err := rebaseRuntimeControl(config); err != nil {
				t.Fatal(err)
			}
			value, err := config.App[0].GetInstance()
			if err != nil {
				t.Fatal(err)
			}
			address := socket
			if socket[0] != '/' {
				address = "@" + address
			}
			for i, rule := range value.(*router.Config).Rule {
				expected := urls[i]
				if i < 2 {
					expected = address + ":/internal/webhook?token=new-token"
				}
				if rule.GetWebhook().Url != expected {
					t.Fatalf("webhook %d: got %q, want %q", i, rule.GetWebhook().Url, expected)
				}
			}
		})
	}
}
