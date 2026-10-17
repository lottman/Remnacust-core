//go:build !wasm

package command_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/app/router"
	routercontrol "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	configserial "github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const runtimeTestUUID = "a4ac647a-075c-4b91-96e9-9fd341fba009"

func availableRuntimePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func runtimeEcho(t *testing.T, prefix string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return runtimeServeEcho(t, listener, prefix)
}

func runtimeServeEcho(t *testing.T, listener net.Listener, prefix string) int {
	t.Helper()
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buffer := make([]byte, 1024)
				for {
					n, err := conn.Read(buffer)
					if err != nil {
						return
					}
					if _, err = conn.Write(append([]byte(prefix), buffer[:n]...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func runtimeTestConfig(t *testing.T, apiPort, port, destination int, second int) *core.Config {
	t.Helper()
	inbounds := []map[string]interface{}{{"tag": "PORT-A", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{"decryption": "none", "clients": []map[string]interface{}{{"id": runtimeTestUUID, "email": "runtime-user"}}}}}
	if second > 0 {
		extra := map[string]interface{}{}
		for key, value := range inbounds[0] {
			extra[key] = value
		}
		extra["tag"] = "PORT-B"
		extra["port"] = second
		inbounds = append(inbounds, extra)
	}
	document := map[string]interface{}{"log": map[string]interface{}{"loglevel": "none"}, "api": map[string]interface{}{"tag": "PRIVATE_API", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService", "RoutingService"}}, "inbounds": inbounds, "outbounds": []map[string]interface{}{{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []map[string]interface{}{{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}}}
	raw, _ := json.Marshal(document)
	config, err := configserial.DecodeJSONConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	built, err := config.Build()
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func runtimeClient(t *testing.T, port, destination int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	id, _ := hex.DecodeString(strings.ReplaceAll(runtimeTestUUID, "-", ""))
	request := append([]byte{0}, id...)
	request = append(request, 0, 1, byte(destination>>8), byte(destination), 1, 127, 0, 0, 1)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	return conn
}

func runtimePing(t *testing.T, conn net.Conn, prefix string, initial bool) {
	t.Helper()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if initial {
		header := make([]byte, 2)
		if _, err := io.ReadFull(conn, header); err != nil {
			t.Fatal(err)
		}
		if header[0] != 0 || header[1] != 0 {
			t.Fatalf("invalid VLESS response: %v", header)
		}
	}
	response := make([]byte, len(prefix)+4)
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != prefix+"ping" {
		t.Fatalf("wrong routing: %q", response)
	}
}

func runtimeInspect(t *testing.T, client *grpc.ClientConn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response := new(structpb.Struct)
	if err := client.Invoke(ctx, control.RuntimeInspectMethod, &structpb.Struct{}, response); err != nil {
		t.Fatal(err)
	}
	return response.AsMap()["hash"].(string)
}

func runtimeApply(client *grpc.ClientConn, config *core.Config, revision string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "if-match", revision)
	return client.Invoke(ctx, control.RuntimeApplyMethod, config, new(structpb.Struct))
}

func TestRuntimeNativePortsKeepSessionsAndRollbackFailedBind(t *testing.T) {
	first, second := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	apiPort, port, extra := availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t)
	config := runtimeTestConfig(t, apiPort, port, first, 0)
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	old := runtimeClient(t, port, first)
	runtimePing(t, old, "A:", true)
	next := runtimeTestConfig(t, apiPort, port, second, extra)
	if err := runtimeApply(client, next, runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, old, "A:", false)
	fresh := runtimeClient(t, port, first)
	runtimePing(t, fresh, "B:", true)
	other := runtimeClient(t, extra, first)
	runtimePing(t, other, "B:", true)
	if err := runtimeApply(client, config, "stale"); status.Code(err) != codes.Aborted {
		t.Fatalf("stale update was not rejected: %v", err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	failed := runtimeTestConfig(t, apiPort, port, first, occupied.Addr().(*net.TCPAddr).Port)
	failed.Inbound[1].Tag = "PORT-C"
	failed.Inbound = append(failed.Inbound, next.Inbound[1])
	before := runtimeInspect(t, client)
	if err := runtimeApply(client, failed, before); err == nil {
		t.Fatal("occupied port was reported as applied")
	}
	if runtimeInspect(t, client) != before {
		t.Fatal("failed bind changed the active revision")
	}
	runtimePing(t, old, "A:", false)
	runtimePing(t, fresh, "B:", false)
	runtimePing(t, other, "B:", false)
	if err := runtimeApply(client, runtimeTestConfig(t, apiPort, port, second, 0), runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, other, "B:", false)
	runtimePing(t, fresh, "B:", false)
}

func TestRuntimeClassicAPIUsesAppliedRoutingAndPersistsChanges(t *testing.T) {
	first, second := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	apiPort, port := availableRuntimePort(t), availableRuntimePort(t)
	config := runtimeTestConfig(t, apiPort, port, first, 0)
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(t.TempDir(), "private", "checkpoint.pb")
	server.SetRuntimeJournal(checkpoint)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	connection, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := runtimeApply(connection, runtimeTestConfig(t, apiPort, port, second, 0), runtimeInspect(t, connection)); err != nil {
		t.Fatal(err)
	}
	pinned := runtimeClient(t, port, first)
	runtimePing(t, pinned, "B:", true)
	handler := control.NewHandlerServiceClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	added := proto.Clone(config.Outbound[0]).(*core.OutboundHandlerConfig)
	added.Tag = "added-through-classic-api"
	if _, err := handler.AddOutbound(ctx, &control.AddOutboundRequest{Outbound: added}); err != nil {
		t.Fatal(err)
	}
	outbounds, err := handler.ListOutbounds(ctx, &control.ListOutboundsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, outbound := range outbounds.Outbounds {
		if outbound.Tag == added.Tag {
			found = true
		}
	}
	if !found {
		t.Fatal("classic API read a retired outbound manager")
	}
	routing := routercontrol.NewRoutingServiceClient(connection)
	rules := &router.Config{Rule: []*router.RoutingRule{{RuleTag: "port-specific", InboundTag: []string{"PORT-A"}, TargetTag: &router.RoutingRule_Tag{Tag: added.Tag}}}}
	if _, err := routing.AddRule(ctx, &routercontrol.AddRuleRequest{Config: serial.ToTypedMessage(rules), ShouldAppend: true}); err != nil {
		t.Fatal(err)
	}
	fresh := runtimeClient(t, port, first)
	runtimePing(t, fresh, "A:", true)
	runtimePing(t, pinned, "B:", false)
	saved, err := core.LoadRuntimeJournal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Outbound) != 2 {
		t.Fatal("classic outbound mutation was absent from restart checkpoint")
	}
	listed, err := routing.ListRule(ctx, &routercontrol.ListRuleRequest{})
	if err != nil || len(listed.Rules) != 1 || listed.Rules[0].RuleTag != "port-specific" {
		t.Fatalf("routing API read retired rules: %v", err)
	}
	if _, err := routing.RemoveRule(ctx, &routercontrol.RemoveRuleRequest{RuleTag: "port-specific"}); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.RemoveOutbound(ctx, &control.RemoveOutboundRequest{Tag: added.Tag}); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, fresh, "A:", false)
	runtimePing(t, runtimeClient(t, port, first), "B:", true)
}

func runtimeUDPEcho(t *testing.T, prefix string) int {
	t.Helper()
	connection, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	go func() {
		buffer := make([]byte, 1024)
		for {
			n, peer, err := connection.ReadFrom(buffer)
			if err != nil {
				return
			}
			if _, err := connection.WriteTo(append([]byte(prefix), buffer[:n]...), peer); err != nil {
				return
			}
		}
	}()
	return connection.LocalAddr().(*net.UDPAddr).Port
}

func runtimeUDPConfig(t *testing.T, apiPort, port, destination int) *core.Config {
	t.Helper()
	config := runtimeTestConfig(t, apiPort, port, destination, 0)
	fragment := fmt.Sprintf(`{"inbounds":[{"tag":"UDP-A","listen":"127.0.0.1","port":%d,"protocol":"dokodemo-door","settings":{"address":"127.0.0.1","port":%d,"network":"udp"}}]}`, port, destination)
	parsed, err := configserial.DecodeJSONConfig(strings.NewReader(fragment))
	if err != nil {
		t.Fatal(err)
	}
	built, err := parsed.Build()
	if err != nil {
		t.Fatal(err)
	}
	config.Inbound = built.Inbound
	return config
}

func TestRuntimeNativeUDPAssociationsKeepTheirRoutingGeneration(t *testing.T) {
	first, second := runtimeUDPEcho(t, "A:"), runtimeUDPEcho(t, "B:")
	apiPort := availableRuntimePort(t)
	reservation, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.LocalAddr().(*net.UDPAddr).Port
	reservation.Close()
	config := runtimeUDPConfig(t, apiPort, port, first)
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	connect := func() net.Conn {
		conn, err := net.Dial("udp", fmt.Sprint("127.0.0.1:", port))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	ping := func(conn net.Conn, prefix string) {
		t.Helper()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 100)
		n, err := conn.Read(buffer)
		if err != nil || string(buffer[:n]) != prefix+"ping" {
			t.Fatalf("UDP route mixed or lost: %q %v", buffer[:n], err)
		}
	}
	old := connect()
	ping(old, "A:")
	next := runtimeUDPConfig(t, apiPort, port, first)
	next.Outbound = runtimeTestConfig(t, apiPort, port, second, 0).Outbound
	if err := runtimeApply(client, next, runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	ping(old, "A:")
	fresh := connect()
	ping(fresh, "B:")
	for i := 0; i < 10; i++ {
		ping(old, "A:")
		ping(fresh, "B:")
	}
}
