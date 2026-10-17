//go:build !wasm

package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xtls/xray-core/core"
)

func TestInspectCheckpointIsReadOnlyAndRejectsCorruption(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "checkpoint.pb")
	if _, err := inspectCheckpoint(file); !os.IsNotExist(err) {
		t.Fatal("missing checkpoint accepted", err)
	}
	config := new(core.Config)
	if err := core.SaveRuntimeJournal(file, config); err != nil {
		t.Fatal(err)
	}
	if required, err := inspectCheckpoint(file); err != nil || required {
		t.Fatal("plain checkpoint changed", err)
	}
	if err := core.RequireUserQuotaConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveRuntimeJournal(file, config); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if required, err := inspectCheckpoint(file); err != nil || !required {
		t.Fatal("mandatory quota ignored", err)
	}
	after, err := os.ReadFile(file)
	if err != nil || string(before) != string(after) {
		t.Fatal("inspection changed checkpoint", err)
	}
	if err := os.WriteFile(file, append(before, byte(1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectCheckpoint(file); err == nil {
		t.Fatal("corrupt checkpoint accepted")
	}
}
