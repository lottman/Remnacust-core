package xdrive

import (
	"context"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"testing"
)

func TestDialKeepsLegacySegmentsByDefault(t *testing.T) {
	newFakeDrive(t)
	config := &Config{RemoteFolder: "folder-id", Service: "google-drive", Secrets: []string{"client", "secret", "refresh"}}
	conn, err := Dial(context.Background(), net.Destination{}, &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := conn.(*Conn).writer.(*walWriter); !ok {
		t.Fatalf("legacy default changed to %T", conn.(*Conn).writer)
	}
}
