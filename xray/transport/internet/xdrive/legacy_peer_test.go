package xdrive

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

func TestDialPreservesLegacyDrivePeer(t *testing.T) {
	drive := newFakeDrive(t)
	settings := driveSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, net.Destination{}, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	storage := newDriveBackend(t)
	defer storage.Close()
	entries, err := storage.List(ctx, sessionsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("announcement: %v, %v", entries, err)
	}
	if len(entries[0].Inline) != 0 || drive.count() != 1 {
		t.Fatal("a legacy peer must receive one empty announcement, without ring slots")
	}
	session, _, ok := parseAnnounce(entries[0].Name)
	if !ok {
		t.Fatal("invalid legacy announcement")
	}
	// This is the pre-slot server session constructor. No listener-side ring
	// support participates in the exchange.
	server := newConn(ctx, storage, downlinkPrefix(session), uplinkPrefix(session), paramsFromConfig(settings.ProtocolSettings.(*Config)), func() {})
	defer server.Close()
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	server.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4)
	if _, err := io.ReadFull(server, data); err != nil || string(data) != "ping" {
		t.Fatalf("legacy uplink: %q, %v", data, err)
	}
	if _, err := server.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, data); err != nil || string(data) != "pong" {
		t.Fatalf("legacy downlink: %q, %v", data, err)
	}
}
