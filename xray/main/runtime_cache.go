package main

import (
	"os"
	"strings"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
)

// Only ephemeral, node-owned control addresses are rebased. Public listeners,
// client credentials and the persisted routing graph remain unchanged.
func rebaseRuntimeControl(config *core.Config) error {
	apiPath, socket, token := os.Getenv("XTLS_API_SOCKET_PATH"), os.Getenv("INTERNAL_SOCKET_PATH"), os.Getenv("INTERNAL_REST_TOKEN")
	for _, inbound := range config.Inbound {
		if inbound.Tag != "REMNAWAVE_API" || apiPath == "" {
			continue
		}
		settings, err := inbound.ReceiverSettings.GetInstance()
		if err != nil {
			return err
		}
		receiver, ok := settings.(*proxyman.ReceiverConfig)
		if !ok {
			continue
		}
		receiver.Listen = net.NewIPOrDomain(net.DomainAddress(apiPath))
		inbound.ReceiverSettings = serial.ToTypedMessage(receiver)
	}
	if socket == "" || token == "" {
		return nil
	}
	for _, app := range config.App {
		if app.Type != serial.GetMessageType(&router.Config{}) {
			continue
		}
		settings, err := app.GetInstance()
		if err != nil {
			return err
		}
		routing := settings.(*router.Config)
		for _, rule := range routing.Rule {
			webhook := rule.GetWebhook()
			if webhook == nil || !strings.HasPrefix(webhook.Url, "@") {
				continue
			}
			separator := strings.Index(webhook.Url, ":/internal/")
			if separator < 0 {
				continue
			}
			path := webhook.Url[separator+1:]
			query := strings.Index(path, "?token=")
			if query < 0 {
				continue
			}
			webhook.Url = "@" + socket + ":" + path[:query] + "?token=" + token
		}
		app.Value = serial.ToTypedMessage(routing).Value
	}
	return nil
}
