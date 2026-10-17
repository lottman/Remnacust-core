//go:build !wasm

package api_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/main/commands/base"
	_ "github.com/xtls/xray-core/main/distro/all"
)

func TestProfileCLIProcess(t *testing.T) {
	raw := os.Getenv("REMNACUST_PROFILE_CLI_TEST_ARGS")
	if raw == "" {
		t.Skip("CLI process fixture")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{os.Args[0]}, args...)
	flag.CommandLine = flag.NewFlagSet("native-profile-cli", flag.ContinueOnError)
	base.Execute()
}

func runProfileCLI(t *testing.T, args ...string) ([]byte, []byte, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, executable, "-test.run=^TestProfileCLIProcess$", "-test.timeout=20s")
	encoded, _ := json.Marshal(args)
	process.Env = append(os.Environ(), "REMNACUST_PROFILE_CLI_TEST_ARGS="+string(encoded))
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	err = process.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func cliPort(t *testing.T) int {
	t.Helper()
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := socket.Addr().(*net.TCPAddr).Port
	socket.Close()
	return port
}

func cliEcho(t *testing.T, prefix string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				buffer := make([]byte, 128)
				for {
					n, err := connection.Read(buffer)
					if err != nil {
						return
					}
					if _, err := connection.Write(append([]byte(prefix), buffer[:n]...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func cliClient(t *testing.T, port, destination int) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", fmt.Sprint("127.0.0.1:", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	connection.SetDeadline(time.Now().Add(20 * time.Second))
	id, _ := hex.DecodeString("a4ac647a075c4b9196e99fd341fba009")
	request := append([]byte{0}, id...)
	request = append(request, 0, 1, byte(destination>>8), byte(destination), 1, 127, 0, 0, 1)
	if _, err := connection.Write(request); err != nil {
		t.Fatal(err)
	}
	return connection
}

func cliPing(t *testing.T, connection net.Conn, prefix string, first bool) {
	t.Helper()
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if first {
		header := make([]byte, 2)
		if _, err := io.ReadFull(connection, header); err != nil {
			t.Fatal(err)
		}
	}
	response := make([]byte, len(prefix)+4)
	if _, err := io.ReadFull(connection, response); err != nil || string(response) != prefix+"ping" {
		t.Fatalf("wrong CLI-applied route %q: %v", response, err)
	}
}

func TestProfileCLIActualDispatchCacheLiveApplyAndLegacyGuard(t *testing.T) {
	a, b := cliEcho(t, "A:"), cliEcho(t, "B:")
	port, apiPort := cliPort(t), cliPort(t)
	const binding = "11111111-1111-4111-8111-111111111111"
	graph := func(destination int) map[string]any {
		return map[string]any{
			"log":       map[string]any{"loglevel": "none"},
			"inbounds":  []any{map[string]any{"tag": "SAME", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": "a4ac647a-075c-4b91-96e9-9fd341fba009", "email": "cli-user"}}}}},
			"outbounds": []any{map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		}
	}
	root := map[string]any{"log": map[string]any{"loglevel": "none"}, "stats": map[string]any{}, "api": map[string]any{"tag": "CONTROL", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}}, "outbounds": []any{map[string]any{"tag": "root", "protocol": "freedom"}}}
	delivery := map[string]any{"epoch": "1", "owner": binding, "revision": "1", "enabled": true, "inputsHash": strings.Repeat("a", 64)}
	entry := map[string]any{"bindingUuid": binding, "configProfileUuid": binding, "configProfileInboundUuid": binding, "config": graph(a)}
	input := map[string]any{"version": "remnacust-profile-input-v1", "root": root, "delivery": delivery, "graphs": []any{entry}}
	directory := t.TempDir()
	inputFile := filepath.Join(directory, "input.json")
	checkpoint := filepath.Join(directory, "state", "checkpoint.pb")
	writeInput := func() {
		data, _ := json.Marshal(input)
		if err := os.WriteFile(inputFile, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeInput()
	stdout, stderr, err := runProfileCLI(t, "api", "cache-profiles", "--output="+checkpoint, inputFile)
	if err != nil {
		t.Fatalf("actual CLI cache dispatch failed: %v %s", err, stderr)
	}
	var ack map[string]any
	if err := json.Unmarshal(stdout, &ack); err != nil || ack["graphCount"] != float64(1) {
		t.Fatal("cache acknowledgement is not machine-readable", string(stdout), err)
	}
	config, err := core.LoadRuntimeJournal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	server, err := core.NewProfile(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	server.SetRuntimeJournal(checkpoint)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	old := cliClient(t, port, a)
	cliPing(t, old, "A:", true)
	address := "--server=" + fmt.Sprint("127.0.0.1:", apiPort)
	stdout, stderr, err = runProfileCLI(t, "api", "profile-inspect", address, "--timeout=5")
	if err != nil {
		t.Fatalf("actual CLI inspect failed: %v %s", err, stderr)
	}
	if err := json.Unmarshal(stdout, &ack); err != nil || ack["dynamicMembership"] != false {
		t.Fatal("inspection response changed", err)
	}
	delivery["epoch"] = "2"
	delivery["revision"] = "2"
	entry["config"] = graph(b)
	writeInput()
	stdout, stderr, err = runProfileCLI(t, "api", "profile-apply", address, "--timeout=5", inputFile)
	if err != nil {
		t.Fatalf("actual CLI live application failed: %v %s", err, stderr)
	}
	if err := json.Unmarshal(stdout, &ack); err != nil || ack["graphCount"] != float64(1) || ack["unchanged"] != false {
		t.Fatal("live acknowledgement changed", err)
	}
	cliPing(t, old, "A:", false)
	cliPing(t, cliClient(t, port, a), "B:", true)
	before, _ := os.ReadFile(checkpoint)
	legacyFile := filepath.Join(directory, "legacy.json")
	legacy, _ := json.Marshal(root)
	if err := os.WriteFile(legacyFile, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runProfileCLI(t, "api", "cache-runtime", "--output="+checkpoint, legacyFile)
	if err == nil || !strings.Contains(string(stderr), "independent profile checkpoint") {
		t.Fatalf("legacy dispatcher did not enforce profile protection: %v %s", err, stderr)
	}
	after, _ := os.ReadFile(checkpoint)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected legacy CLI changed the profile journal")
	}
	stdout, stderr, err = runProfileCLI(t, "api", "inspect-checkpoint", "--input="+checkpoint)
	if err != nil {
		t.Fatalf("legacy checkpoint inspection route failed: %v %s", err, stderr)
	}
	if err := json.Unmarshal(stdout, &ack); err != nil || ack["userQuotaRequired"] != false {
		t.Fatal("checkpoint inspection acknowledgement changed", err)
	}
	_, stderr, err = runProfileCLI(t, "api", "require-user-quota", address, "--timeout=5", "--core=not-this-core")
	if err == nil || !strings.Contains(string(stderr), "Core incarnation changed") {
		t.Fatalf("quota CLI did not reach its native RPC: %v %s", err, stderr)
	}
}
