package main

import (
	"os"
	"regexp"
	"strings"

	"github.com/xtls/xray-core/app/commander"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
)

var privateInternalWebhook = regexp.MustCompile(`^/run/remnacust-xray-[a-zA-Z0-9]{10}/internal\.sock:/internal/`)

// Only ephemeral, node-owned control addresses are rebased. Public listeners,
// client credentials and the persisted routing graph remain unchanged.
func rebaseRuntimeControl(config *core.Config) error {
	apiPath, socket, token := os.Getenv("XTLS_API_SOCKET_PATH"), os.Getenv("INTERNAL_SOCKET_PATH"), os.Getenv("INTERNAL_REST_TOKEN")
	for _, inbound := range config.Inbound {
		if (inbound.Tag != "REMNAWAVE_API_INBOUND" && inbound.Tag != "REMNAWAVE_API") || apiPath == "" {
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
		receiver.Listen = net.NewIPOrDomain(net.DomainAddress(apiPath + ",0600"))
		inbound.ReceiverSettings = serial.ToTypedMessage(receiver)
	}
	for _, app := range config.App {
		if apiPath != "" && app.Type == serial.GetMessageType(&commander.Config{}) {
			settings, err := app.GetInstance()
			if err != nil {
				return err
			}
			control := settings.(*commander.Config)
			if control.Tag == "REMNAWAVE_API" && control.Listen != "" {
				control.Listen = apiPath + ",0600"
				app.Value = serial.ToTypedMessage(control).Value
			}
		}
		if socket == "" || token == "" || app.Type != serial.GetMessageType(&router.Config{}) {
			continue
		}
		settings, err := app.GetInstance()
		if err != nil {
			return err
		}
		routing := settings.(*router.Config)
		for _, rule := range routing.Rule {
			webhook := rule.GetWebhook()
			if webhook == nil || (!strings.HasPrefix(webhook.Url, "@") && !privateInternalWebhook.MatchString(webhook.Url)) {
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
			address := socket
			if !strings.HasPrefix(address, "/") {
				address = "@" + address
			}
			webhook.Url = address + ":" + path[:query] + "?token=" + token
		}
		app.Value = serial.ToTypedMessage(routing).Value
	}
	return nil
}
