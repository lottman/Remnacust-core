package xerahttp

import (
	"context"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"testing"
)

func TestRuntimeRetirementRemovesItsHTTPPool(t *testing.T) {
	settings := &internet.MemoryStreamConfig{ProtocolSettings: &Config{Path: "/runtime-test"}}
	destination := net.TCPDestination(net.LocalHostIP, 1)
	client, lease := getHTTPClient(context.Background(), destination, settings)
	settings.Close()
	globalDialerAccess.Lock()
	_, present := globalDialerMap[dialerConf{destination, settings}]
	globalDialerAccess.Unlock()
	if present || client.IsClosed() {
		t.Fatal("retired XeraHTTP pool remained cached or interrupted its active stream")
	}
	lease.DoneRunning()
	if !client.IsClosed() {
		t.Fatal("retired XeraHTTP pool remained open after its last stream")
	}
	retired, _ := getHTTPClient(context.Background(), destination, settings)
	if !retired.IsClosed() {
		t.Fatal("retired XeraHTTP settings created another pool")
	}
}
