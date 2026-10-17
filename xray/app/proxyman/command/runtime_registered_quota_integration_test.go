//go:build !wasm

package command_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// This fixture owns a fresh container and the actual private policy/journal
// paths. It exercises a real VLESS TCP stream, not a mocked debit callback.
func TestRuntimeRegisteredQuotaIsolated(t *testing.T) {
	if os.Getenv("REMNACUST_ISOLATED_REGISTERED_QUOTA_TEST") != "1" {
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
	const id = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const cancelled = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	payload := json.RawMessage(`{"instance":"` + instance + `","version":"remnacust-quota-reserves-v1","entries":{}}`)
	sum := sha256.Sum256(payload)
	data, _ := json.Marshal(map[string]any{"payload": payload, "sha256": hex.EncodeToString(sum[:])})
	if err := os.WriteFile(filepath.Join(root, "quota/ledger.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	grant := func(value string) map[string]any {
		return map[string]any{"id": value, "instance": instance, "ceiling": "100", "expiresAt": time.Now().Add(time.Hour).Truncate(time.Second).UnixMilli(), "registrationRequired": true}
	}
	activeGrant, cancelledGrant := grant(id), grant(cancelled)
	group := map[string]any{"multiplierScale": 1000000000, "reserveRequired": true, "reserves": map[string]any{}}
	policy := map[string]any{
		"version": "xera-host-policy-v2", "generation": "registered-initial", "offlineReserves": "remnacust-quota-reserves-v1", "quotaRegistrations": "remnacust-quota-registrations-v1", "userQuota": "remnacust-user-quota-reserves-v1",
		"expiresAt": 1, "protectedInbounds": []string{}, "hosts": map[string]any{}, "legacyGroups": []string{"user:global"},
		"legacyOwners": map[string]any{"123": map[string]any{"allowed": true, "expiresAt": 0}}, "legacyQuotaMultipliers": map[string]int64{"user:global": 1000000000},
		"groups":        map[string]any{"user:global": group},
		"registrations": []any{map[string]any{"grant": activeGrant, "group": "user:global", "owner": "123", "scale": 1000000000, "action": "prepare"}},
	}
	write := func(generation string) {
		policy["generation"] = generation
		journal, err := os.Open(filepath.Join(root, "quota/ledger.json"))
		if err != nil {
			t.Fatal(err)
		}
		reader := json.NewDecoder(journal)
		var sequence uint64
		for {
			var envelope struct {
				Payload struct {
					Sequence uint64 `json:"sequence"`
				} `json:"payload"`
			}
			err := reader.Decode(&envelope)
			if err == io.EOF {
				break
			}
			if err != nil {
				journal.Close()
				t.Fatal(err)
			}
			sequence = envelope.Payload.Sequence
		}
		journal.Close()
		policy["quotaCheckpoint"] = map[string]any{"instance": instance, "sequence": fmt.Sprint(sequence)}
		body, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(root, "host-policy.tmp")
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(file, filepath.Join(root, "host-policy.json")); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(generation string) {
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			body, _ := os.ReadFile(filepath.Join(root, "host-policy.status.json"))
			var state map[string]any
			if json.Unmarshal(body, &state) == nil && state["generation"] == generation && state["quotaRegistrations"] == "remnacust-quota-registrations-v1" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("native policy ACK timeout", generation)
	}
	write("registered-initial")
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
	server.SetRuntimeJournal(filepath.Join(t.TempDir(), "private/checkpoint.pb"))
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	wait("registered-initial")
	connection, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, _ := structpb.NewStruct(map[string]any{"coreProcessId": core.ProcessID()})
	if err := connection.Invoke(ctx, control.RuntimeRequireUserQuotaMethod, request, new(structpb.Struct)); err != nil {
		t.Fatal(err)
	}
	blocked := runtimeClient(t, port, destination)
	blocked.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = blocked.Write([]byte("ping"))
	if _, err := io.ReadFull(blocked, make([]byte, 6)); err == nil {
		t.Fatal("prepared credit admitted traffic")
	}
	blocked.Close()
	delete(policy, "registrations")
	group["reserves"] = map[string]any{"123": activeGrant}
	write("registered-active")
	wait("registered-active")
	stream := runtimeClient(t, port, destination)
	runtimePing(t, stream, "", true)
	// Cancelling a different never-delivered grant must not disturb this stream.
	policy["registrations"] = []any{map[string]any{"grant": cancelledGrant, "group": "user:global", "owner": "123", "scale": 1000000000, "action": "cancel", "allowMissing": true}}
	write("registered-cancel-unused")
	wait("registered-cancel-unused")
	runtimePing(t, stream, "", false)
	group["reserves"] = map[string]any{}
	policy["registrations"] = []any{map[string]any{"grant": activeGrant, "group": "user:global", "owner": "123", "scale": 1000000000, "action": "cancel", "allowMissing": false}}
	write("registered-cancel-used")
	wait("registered-cancel-used")
	stream.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = stream.Write([]byte("ping"))
	if _, err := io.ReadFull(stream, make([]byte, 4)); err == nil {
		t.Fatal("closed credit admitted another packet")
	}
	stream.Close()
	// Fold actual durable frames to verify exact zero versus used closure.
	file, err := os.Open(filepath.Join(root, "quota/ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	entries := map[string]map[string]any{}
	for {
		var record struct {
			Payload struct {
				Entries map[string]map[string]any `json:"entries"`
				Version string                    `json:"version"`
			} `json:"payload"`
		}
		err := decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range record.Payload.Entries {
			entries[key] = value
		}
	}
	if e := entries[cancelled]; e["closed"] != true || e["reserved"] != float64(0) || e["prepared"] != true {
		t.Fatal("unused tombstone", e)
	}
	if e := entries[id]; e["closed"] != true || e["reserved"] != float64(16) || e["activated"] != true {
		t.Fatal("used closure lost actual charge", e)
	}
	file.Close()
	connection.Close()
	server.Close()
	if err := os.WriteFile(filepath.Join(root, "quota/ledger.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(os.Args[0], "-test.run=^TestRuntimeQuotaCheckpointStartupHelper$", "-test.count=1")
	process.Env = append(os.Environ(), "REMNACUST_QUOTA_CHECKPOINT_STARTUP_HELPER=1")
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("restored journal startup guard failed: %v %s", err, output)
	}
}

func TestRuntimeQuotaCheckpointStartupHelper(t *testing.T) {
	if os.Getenv("REMNACUST_QUOTA_CHECKPOINT_STARTUP_HELPER") != "1" {
		t.Skip("isolated restart helper")
	}
	if os.Getenv("REMNACUST_ISOLATED_REGISTERED_QUOTA_TEST") != "1" {
		t.Fatal("requires isolated parent")
	}
	config := runtimeBuildJSON(t, map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": []any{map[string]any{"tag": "PUBLIC", "listen": "127.0.0.1", "port": availableRuntimePort(t), "protocol": "vless", "settings": map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": runtimeTestUUID, "email": "123"}}}}}, "outbounds": []any{map[string]any{"protocol": "freedom"}}})
	server, err := core.New(config)
	if err != nil {
		if !strings.Contains(err.Error(), "checkpoint") {
			t.Fatal("unrelated startup failure", err)
		}
		return
	}
	defer server.Close()
	if err := server.Start(); err == nil {
		t.Fatal("restored old journal opened public listeners")
	} else if !strings.Contains(err.Error(), "checkpoint") {
		t.Fatal("unrelated startup failure", err)
	}
}
