//go:build !wasm

package command_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

// This test owns the fixed node policy path. It must run in a fresh disposable
// container, never against a running node's /var/lib/remnanode directory.
func TestRuntimeOfflineQuotaIsolated(t *testing.T) {
	if os.Getenv("REMNACUST_ISOLATED_QUOTA_TEST") != "1" {
		t.Skip("requires the dedicated disposable quota test container")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Fatal("quota fixture requires a disposable Docker container")
	}
	const root = "/var/lib/remnanode"
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatalf("refuse existing node state: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Mkdir(filepath.Join(root, "quota"), 0700); err != nil {
		t.Fatal(err)
	}
	const instance = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const host = "11111111111141118111111111111111"
	const email = "123~0123456789abcdef01234567~h" + host
	payload := json.RawMessage(`{"instance":"` + instance + `","version":"remnacust-quota-reserves-v1","entries":{}}`)
	sum := sha256.Sum256(payload)
	journal, _ := json.Marshal(map[string]any{"payload": payload, "sha256": hex.EncodeToString(sum[:])})
	if err := os.WriteFile(filepath.Join(root, "quota/ledger.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	grant := map[string]any{"id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "instance": instance, "ceiling": "16", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}
	groups := map[string]any{"host:fixture": map[string]any{"reserveRequired": true, "reserves": map[string]any{"123": grant}, "blockedOwners": map[string]bool{}, "bytesPerSecond": 0, "totalBytesPerSecond": 0, "blockAll": false}}
	policy := map[string]any{
		"version": "xera-host-policy-v2", "offlineReserves": "remnacust-quota-reserves-v1",
		// Deliberately stale panel TTL: only the finite local reserve and identity
		// deadline authorize continued traffic while the panel is unavailable.
		"expiresAt": 1, "protectedInbounds": []string{"PORT-A"}, "groups": groups,
		"hosts": map[string]any{host: map[string]any{"inboundTag": "PORT-A", "allowedIdentities": map[string]bool{email: true}, "identityExpiresAt": map[string]int{email: 0}, "quotaMultipliers": map[string]int{"host:fixture": 50}, "groups": []string{"host:fixture"}, "domainMode": "OFF", "domains": []string{}}},
	}
	coreStarted := false
	writePolicy := func(generation string) {
		t.Helper()
		policy["generation"] = generation
		data, _ := json.Marshal(policy)
		tmp := filepath.Join(root, "fixture.tmp")
		if err := os.WriteFile(tmp, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(root, "host-policy.json")); err != nil {
			t.Fatal(err)
		}
		// Before startup this is a staged policy. Importing a CLI package must
		// never create an acknowledgement on behalf of a not-yet-running core.
		if !coreStarted {
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(filepath.Join(root, "host-policy.status.json"))
			var status map[string]any
			if json.Unmarshal(data, &status) == nil && status["generation"] == generation {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("core did not acknowledge quota policy")
	}
	writePolicy("quota-start")
	port, otherPort, destination := availableRuntimePort(t), availableRuntimePort(t), runtimeEcho(t, "")
	config := runtimeBuildJSON(t, map[string]any{
		"log": map[string]any{"loglevel": "none"},
		"inbounds": []any{
			map[string]any{"tag": "PORT-A", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": runtimeTestUUID, "email": email}}}},
			map[string]any{"tag": "PORT-B", "listen": "127.0.0.1", "port": otherPort, "protocol": "vless", "settings": map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": runtimeTestUUID, "email": "unrelated"}}}},
		},
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
	coreStarted = true
	initialStatus, err := os.ReadFile(filepath.Join(root, "host-policy.status.json"))
	var initialAck map[string]any
	if err != nil || json.Unmarshal(initialStatus, &initialAck) != nil || initialAck["generation"] != "quota-start" ||
		initialAck["coreIncarnation"] != "remnacust-core-incarnation-v1" || initialAck["coreProcessId"] == nil {
		t.Fatalf("startup did not load and acknowledge the saved quota policy: %v: %s", err, initialStatus)
	}
	first, second := runtimeClient(t, port, destination), runtimeClient(t, port, destination)
	other := runtimeClient(t, otherPort, destination)
	runtimePing(t, first, "", true)
	groups["host:fixture"].(map[string]any)["fenceAll"] = true
	writePolicy("quota-paused")
	started, pending := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := first.Write([]byte("ping"))
		close(started)
		if err == nil {
			response := make([]byte, 4)
			_, err = io.ReadFull(first, response)
			if err == nil && string(response) != "ping" {
				err = fmt.Errorf("wrong retained response: %q", response)
			}
		}
		pending <- err
	}()
	<-started
	select {
	case err := <-pending:
		t.Fatalf("quota fence closed or bypassed an existing stream: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	runtimePing(t, other, "", true)
	delete(groups["host:fixture"].(map[string]any), "fenceAll")
	writePolicy("quota-resumed")
	if err := <-pending; err != nil {
		t.Fatal("retained stream did not resume", err)
	}
	runtimePing(t, second, "", true)
	runtimePing(t, second, "", false)
	// Four round trips consume 32 raw bytes, billed as 16 at multiplier 0.50,
	// across both authenticated connections and both traffic directions.
	first.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = first.Write([]byte("ping"))
	buffer := make([]byte, 4)
	_, err = io.ReadFull(first, buffer)
	if err == nil {
		t.Fatal("exhausted shared reserve forwarded a fifth response")
	}
	if err, ok := err.(net.Error); ok && err.Timeout() {
		t.Fatal("quota exhaustion hung the connection instead of rejecting it")
	}
	runtimePing(t, other, "", false)
	groups["host:fixture"].(map[string]any)["reserves"] = map[string]any{}
	writePolicy("quota-sealed")
	data, err := os.ReadFile(filepath.Join(root, "quota/ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var last struct {
		Payload struct {
			Entries map[string]struct {
				Closed   bool
				Reserved int64
			}
		}
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) > 0 {
			if err := json.Unmarshal(line, &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	entry := last.Payload.Entries[grant["id"].(string)]
	if !entry.Closed || entry.Reserved != 16 {
		t.Fatalf("closed reserve lost its final charge: %+v", entry)
	}
	runtimePing(t, other, "", false)
}
