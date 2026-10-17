package grpc

import (
	"context"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"google.golang.org/grpc/connectivity"
	"testing"
)

func TestRuntimeRetirementRemovesOnlyItsGRPCPool(t *testing.T) {
	settings := &internet.MemoryStreamConfig{ProtocolSettings: &Config{ServiceName: "runtime-test"}}
	destination := net.TCPDestination(net.LocalHostIP, 1)
	client, err := getGrpcClient(context.Background(), destination, settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.Close()
	globalDialerAccess.Lock()
	_, present := globalDialerMap[dialerConf{destination, settings}]
	globalDialerAccess.Unlock()
	if present || client.GetState() != connectivity.Shutdown {
		t.Fatal("retired gRPC pool remained cached or connected")
	}
	if _, err := getGrpcClient(context.Background(), destination, settings); err == nil {
		t.Fatal("retired gRPC settings created another pool")
	}
}
