//go:build !wasm

package command_test

import (
	"fmt"
	"testing"

	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeEquivalentReceiverEncodingKeepsLiveVlessConnection(t *testing.T) {
	destination := runtimeEcho(t, "A:")
	apiPort, port := availableRuntimePort(t), availableRuntimePort(t)
	config := runtimeTestConfig(t, apiPort, port, destination, 0)
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	control, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	old := runtimeClient(t, port, destination)
	runtimePing(t, old, "A:", true)
	next := proto.Clone(config).(*core.Config)
	wire := next.Inbound[0].ReceiverSettings.Value
	var fields [][]byte
	for len(wire) > 0 {
		_, _, n := protowire.ConsumeField(wire)
		if n < 0 {
			t.Fatal("invalid fixture wire")
		}
		fields = append(fields, wire[:n])
		wire = wire[n:]
	}
	var reversed []byte
	for i := len(fields) - 1; i >= 0; i-- {
		reversed = append(reversed, fields[i]...)
	}
	next.Inbound[0].ReceiverSettings.Value = reversed
	if proto.Equal(config.Inbound[0].ReceiverSettings, next.Inbound[0].ReceiverSettings) {
		t.Fatal("fixture wire order did not change")
	}
	if err = runtimeApply(control, next, runtimeInspect(t, control)); err != nil {
		t.Fatalf("equivalent listener rejected: %v", err)
	}
	runtimePing(t, old, "A:", false)
	runtimePing(t, runtimeClient(t, port, destination), "A:", true)
}
