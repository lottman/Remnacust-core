//go:build !wasm

package command_test

import (
	"fmt"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport/internet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeDialerProxyUsesAppliedGenerationAndKeepsExistingStreams(t *testing.T) {
	first, second := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	apiPort, port := availableRuntimePort(t), availableRuntimePort(t)
	config := runtimeTestConfig(t, apiPort, port, first, 0)
	chain := proto.Clone(config.Outbound[0]).(*core.OutboundHandlerConfig)
	chain.Tag = "chain"
	config.Outbound[0].SenderSettings = serial.ToTypedMessage(&proxyman.SenderConfig{
		StreamSettings: &internet.StreamConfig{SocketSettings: &internet.SocketConfig{DialerProxy: "chain"}},
	})
	config.Outbound = append(config.Outbound, chain)
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	client, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	old := runtimeClient(t, port, first)
	runtimePing(t, old, "A:", true)
	updated := proto.Clone(config).(*core.Config)
	updated.Outbound[1].ProxySettings = runtimeTestConfig(t, apiPort, port, second, 0).Outbound[0].ProxySettings
	if err := runtimeApply(client, updated, runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	fresh := runtimeClient(t, port, first)
	runtimePing(t, fresh, "B:", true)
	runtimePing(t, old, "A:", false)
	if err := runtimeApply(client, config, runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	restored := runtimeClient(t, port, first)
	runtimePing(t, restored, "A:", true)
	runtimePing(t, fresh, "B:", false)
	runtimePing(t, old, "A:", false)
}
