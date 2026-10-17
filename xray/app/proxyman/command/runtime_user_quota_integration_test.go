//go:build !wasm

package command_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/vless"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestRuntimeUserQuotaIsolated(t *testing.T) {
	if os.Getenv("REMNACUST_ISOLATED_USER_QUOTA_TEST") != "1" {
		t.Skip("requires a fresh disposable quota container")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Fatal("requires Docker isolation")
	}
	const root = "/var/lib/remnanode"
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal("refuse existing node state", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Mkdir(filepath.Join(root, "quota"), 0700); err != nil {
		t.Fatal(err)
	}
	const instance = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	payload := json.RawMessage(`{"instance":"` + instance + `","version":"remnacust-quota-reserves-v1","entries":{}}`)
	sum := sha256.Sum256(payload)
	journal, _ := json.Marshal(map[string]any{"payload": payload, "sha256": hex.EncodeToString(sum[:])})
	if err := os.WriteFile(filepath.Join(root, "quota/ledger.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{
		"version": "xera-host-policy-v2", "generation": "global-initial", "offlineReserves": "remnacust-quota-reserves-v1", "userQuota": "remnacust-user-quota-reserves-v1",
		"expiresAt": 1, "protectedInbounds": []string{}, "hosts": map[string]any{},
		"legacyGroups": []string{"user:global"}, "legacyOwners": map[string]any{"123": map[string]any{"allowed": true, "expiresAt": 0}, "124": map[string]any{"allowed": true, "expiresAt": 0}},
		"legacyQuotaMultipliers": map[string]int64{"user:global": 1500000000},
		"groups":                 map[string]any{"user:global": map[string]any{"multiplierScale": 1000000000, "reserveRequired": true, "unlimitedOwners": map[string]bool{"124": true}, "reserves": map[string]any{"123": map[string]any{"id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "instance": instance, "ceiling": "36", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}}}},
	}
	document, _ := json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(root, "host-policy.json"), document, 0600); err != nil {
		t.Fatal(err)
	}
	apiPort, port, destination := availableRuntimePort(t), availableRuntimePort(t), runtimeEcho(t, "")
	config := runtimeBuildJSON(t, map[string]any{
		"log": map[string]any{"loglevel": "none"}, "api": map[string]any{"tag": "PRIVATE_API", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}},
		"inbounds":  []any{map[string]any{"tag": "PORT-A", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": runtimeTestUUID, "email": "123"}}}}},
		"outbounds": []any{map[string]any{"protocol": "freedom", "settings": map[string]any{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
	})
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	connection, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, _ := structpb.NewStruct(map[string]any{"coreProcessId": core.ProcessID()})
	invoke := func() (*structpb.Struct, error) {
		reply := new(structpb.Struct)
		err := connection.Invoke(ctx, control.RuntimeRequireUserQuotaMethod, request, reply)
		return reply, err
	}
	if _, err := invoke(); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("missing native checkpoint was accepted", err)
	}
	checkpoint := filepath.Join(t.TempDir(), "private", "checkpoint.pb")
	server.SetRuntimeJournal(checkpoint)
	const addedUUID = "83d2670f-9bd9-408c-a6c6-b21c1c81a902"
	handler := control.NewHandlerServiceClient(connection)
	_, err = handler.AlterInbound(ctx, &control.AlterInboundRequest{Tag: "PORT-A", Operation: serial.ToTypedMessage(&control.AddUserOperation{User: &protocol.User{Email: "124", Account: serial.ToTypedMessage(&vless.Account{Id: addedUUID})}})})
	if err != nil {
		t.Fatal("dynamic user could not be added", err)
	}
	old := runtimeClient(t, port, destination)
	runtimePing(t, old, "", true)
	reply, err := invoke()
	if err != nil || reply.AsMap()["userQuotaRequired"] != true || reply.AsMap()["coreProcessId"] != core.ProcessID() {
		t.Fatal("mandatory quota was not committed", err)
	}
	cached, err := core.LoadRuntimeJournal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if required, err := core.ConfigRequiresUserQuota(cached); err != nil || !required {
		t.Fatal("checkpoint omitted quota requirement", err)
	}
	runtimePing(t, old, "", false)
	if _, err := invoke(); err != nil {
		t.Fatal("idempotent quota requirement broke the core", err)
	}
	users, err := handler.GetInboundUsers(ctx, &control.GetInboundUserRequest{Tag: "PORT-A"})
	if err != nil || len(users.Users) != 2 {
		t.Fatal("quota commit replaced a dynamically added credential with a stale profile", err)
	}
	// An older profile writer must not be able to remove the sticky metadata.
	for i, app := range cached.App {
		if app.Type == core.UserQuotaRequirementType {
			cached.App = append(cached.App[:i], cached.App[i+1:]...)
			break
		}
	}
	if err := runtimeApply(connection, cached, runtimeInspect(t, connection)); err != nil {
		t.Fatal(err)
	}
	cached, err = core.LoadRuntimeJournal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if required, err := core.ConfigRequiresUserQuota(cached); err != nil || !required {
		t.Fatal("older profile removed quota protection", err)
	}
	runtimePing(t, old, "", false)
	old.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = old.Write([]byte("ping"))
	_, err = io.ReadFull(old, make([]byte, 4))
	if err == nil {
		t.Fatal("fourth round trip exceeded the shared 36-byte weighted reserve")
	}
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("quota exhaustion hung instead of rejecting the stream")
	}
	// The dynamically added unlimited user remains valid on the same port.
	other, err := net.DialTimeout("tcp", fmt.Sprint("127.0.0.1:", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetDeadline(time.Now().Add(3 * time.Second))
	id, _ := hex.DecodeString("83d2670f9bd9408ca6c6b21c1c81a902")
	hello := append([]byte{0}, id...)
	hello = append(hello, 0, 1, byte(destination>>8), byte(destination), 1, 127, 0, 0, 1)
	if _, err := other.Write(hello); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, other, "", true)
	wrong, _ := structpb.NewStruct(map[string]any{"coreProcessId": instance})
	if err := connection.Invoke(ctx, control.RuntimeRequireUserQuotaMethod, wrong, new(structpb.Struct)); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("wrong incarnation accepted", err)
	}
	other.Close()
	old.Close()
	connection.Close()
	server.Close()
	// A separate process has no in-memory policy left to hide the lost files.
	if err := os.Remove(filepath.Join(root, "host-policy.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "quota/user-quota-required")); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeUserQuotaMissingPolicyHelper$")
	child.Env = append(os.Environ(), "REMNACUST_USER_QUOTA_CHECKPOINT_TEST="+checkpoint)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("missing saved policy did not fail before listeners: %v %s", err, output)
	}
}

func TestRuntimeUserQuotaMissingPolicyHelper(t *testing.T) {
	file := os.Getenv("REMNACUST_USER_QUOTA_CHECKPOINT_TEST")
	if file == "" {
		t.Skip("isolated restart helper")
	}
	if os.Getenv("REMNACUST_ISOLATED_USER_QUOTA_TEST") != "1" {
		t.Fatal("refuse nonisolated helper")
	}
	config, err := core.LoadRuntimeJournal(file)
	if err != nil {
		t.Fatal(err)
	}
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.Start(); err == nil {
		t.Fatal("cached credentials started without their required quota policy")
	}
}
