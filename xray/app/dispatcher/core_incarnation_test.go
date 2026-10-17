//go:build !wasm

package dispatcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/uuid"
)

func TestHostPolicyImportsDoNotTouchNodeState(t *testing.T) {
	if os.Getenv("REMNACUST_ISOLATED_HOST_POLICY_START_TEST") != "1" {
		t.Skip("requires an explicitly isolated disposable container")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Fatal("refuse host filesystem")
	}
	const directory = "/var/lib/remnanode"
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatal("refuse existing node state")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.WriteFile(hostPolicyPath, []byte(`{"version":"xera-host-policy-v2","generation":"cli-must-not-load","hosts":{},"groups":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(directory, "host-policy.status.json")
	want := []byte(`{"generation":"running-core-must-keep-this"}`)
	if err := os.WriteFile(status, want, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestCoreIncarnationProcess$")
	command.Env = append(os.Environ(), "REMNACUST_CORE_INCARNATION_CHILD=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("CLI-like process failed: %v: %s", err, output)
	}
	got, err := os.ReadFile(status)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("importing the dispatcher touched the running core ACK: %v: %s", err, got)
	}
}

func TestCoreIncarnationInvalidatesPreviousAcknowledgement(t *testing.T) {
	file := filepath.Join(t.TempDir(), "host-policy.status.json")
	if err := os.WriteFile(file, []byte(`{"pid":1,"generation":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := invalidateHostPolicyStatus(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("a reused PID could still find the previous process acknowledgement")
	}
	if err := invalidateHostPolicyStatus(file); err != nil {
		t.Fatal("first boot without a previous acknowledgement must remain valid:", err)
	}
}

func TestCoreIncarnationProcess(t *testing.T) {
	if os.Getenv("REMNACUST_CORE_INCARNATION_CHILD") == "1" {
		fmt.Println("REMNACUST_CORE_ID=" + coreIncarnation)
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readChild := func() string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestCoreIncarnationProcess$")
		command.Env = append(os.Environ(), "REMNACUST_CORE_INCARNATION_CHILD=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("core process fixture: %v: %s", err, output)
		}
		for _, line := range strings.Split(string(output), "\n") {
			if value, found := strings.CutPrefix(strings.TrimSpace(line), "REMNACUST_CORE_ID="); found {
				if _, err := uuid.ParseString(value); err != nil {
					t.Fatalf("invalid core process ID: %v", err)
				}
				return value
			}
		}
		t.Fatal("core process ID not emitted")
		return ""
	}
	first, second := readChild(), readChild()
	if first == second || first == coreIncarnation || second == coreIncarnation {
		t.Fatal("a fresh core process reused a previous incarnation")
	}
}
