//go:build !wasm

package command_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

func profileUDPEcho(t *testing.T, prefix string) int {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		packet := make([]byte, 2048)
		for {
			n, remote, err := listener.ReadFrom(packet)
			if err != nil {
				return
			}
			_, _ = listener.WriteTo(append([]byte(prefix), packet[:n]...), remote)
		}
	}()
	return listener.LocalAddr().(*net.UDPAddr).Port
}

func profileUDPExchange(t *testing.T, peer net.PacketConn, port int, prefix string) {
	t.Helper()
	if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteTo([]byte("hello"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 128)
	n, _, err := peer.ReadFrom(packet)
	if err != nil || string(packet[:n]) != prefix+"hello" {
		t.Fatalf("native UDP exchange: %q, %v", packet[:n], err)
	}
}

func TestProfileTopologyRPCPreservesNativeUDPFlowOnReusedPort(t *testing.T) {
	a, b := profileUDPEcho(t, "A:"), profileUDPEcho(t, "B:")
	reservation, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.LocalAddr().(*net.UDPAddr).Port
	_ = reservation.Close()
	apiPort := availableRuntimePort(t)
	build := func(destination int) *core.Config {
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":    map[string]interface{}{"loglevel": "none"},
			"policy": map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"connIdle": 2, "uplinkOnly": 0, "downlinkOnly": 0}}},
			"inbounds": []interface{}{map[string]interface{}{"tag": "UDP", "listen": "127.0.0.1", "port": port, "protocol": "tunnel",
				"settings": map[string]interface{}{"address": "127.0.0.1", "port": destination, "network": "udp"}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{
				"finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
	}
	base := runtimeBuildJSON(t, map[string]interface{}{
		"api":   map[string]interface{}{"tag": "CONTROL", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}},
		"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"},
	})
	initial, err := core.WithProfileBindings(base, []core.ProfileBinding{{BindingID: enrolledA, ProfileID: enrolledA, InboundID: enrolledA, Config: build(a)}})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := core.NewProfile(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.SetRuntimeJournal(filepath.Join(t.TempDir(), "private", "udp.pb"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	old := owner.ManagedProfile(enrolledA)
	peerA, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peerA.Close()
	profileUDPExchange(t, peerA, port, "A:")
	rpc, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current := new(structpb.Struct)
	if err := rpc.Invoke(ctx, control.ProfileControlInspectMethod, &structpb.Struct{}, current); err != nil {
		t.Fatal(err)
	}
	target, err := core.WithProfileBindings(base, []core.ProfileBinding{{BindingID: enrolledB, ProfileID: enrolledB, InboundID: enrolledB, Config: build(b)}})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := core.ProfilePlanDigest(target)
	if err != nil {
		t.Fatal(err)
	}
	target, err = core.WithProfileDelivery(target, core.ProfileDelivery{Epoch: "1", Owner: enrolledA, Revision: "1", Enabled: true, InputsHash: strings.Repeat("a", 64), PlanHash: digest})
	if err != nil {
		t.Fatal(err)
	}
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("if-match", current.GetFields()["hash"].GetStringValue()))
	if err := rpc.Invoke(ctx, control.ProfileControlApplyMethod, target, new(structpb.Struct)); err != nil {
		t.Fatal(err)
	}
	profileUDPExchange(t, peerA, port, "A:")
	peerB, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peerB.Close()
	profileUDPExchange(t, peerB, port, "B:")
	select {
	case <-old.RuntimeDone():
		t.Fatal("old native UDP flow was cut during replacement")
	default:
	}
	_ = peerA.Close()
	select {
	case <-old.RuntimeDone():
	case <-time.After(8 * time.Second):
		t.Fatal("native idle close did not release the retired UDP graph")
	}
	profileUDPExchange(t, peerB, port, "B:")
}
