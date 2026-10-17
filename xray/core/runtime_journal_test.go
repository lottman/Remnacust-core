package core

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeJournalRoundTripAndAtomicRejectedReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "checkpoint.pb")
	config := &Config{Inbound: []*InboundHandlerConfig{{Tag: "port-443"}}}
	if err := SaveRuntimeJournal(path, config); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRuntimeJournal(path)
	if err != nil || !proto.Equal(config, loaded) {
		t.Fatalf("checkpoint did not round trip: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oversized := &Config{App: []*serial.TypedMessage{{Type: "invalid", Value: make([]byte, RuntimeJournalLimit)}}}
	if err := SaveRuntimeJournal(path, oversized); err == nil {
		t.Fatal("oversized checkpoint was accepted")
	}
	if err := SaveRuntimeJournal(path, nil); err == nil {
		t.Fatal("nil checkpoint was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed replacement damaged the previous checkpoint")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary checkpoint files leaked: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		parent, _ := os.Stat(filepath.Dir(path))
		if info.Mode().Perm() != 0600 || parent.Mode().Perm() != 0700 {
			t.Fatal("checkpoint credentials were not stored privately")
		}
	}
}

func TestRuntimeJournalRejectsCorruptAndTruncatedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "checkpoint.pb")
	if err := SaveRuntimeJournal(path, &Config{}); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	separator := bytes.Clone(valid)
	separator[len(runtimeJournalMagic)+64] = 'x'
	checksum := bytes.Clone(valid)
	checksum[len(runtimeJournalMagic)] = 'x'
	for _, data := range [][]byte{nil, valid[:len(runtimeJournalMagic)], separator, checksum, append(bytes.Clone(valid), 1)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRuntimeJournal(path); err == nil {
			t.Fatal("corrupt checkpoint was accepted")
		}
	}
	if _, err := LoadRuntimeJournal("relative/checkpoint.pb"); err == nil {
		t.Fatal("relative checkpoint was accepted")
	}
}

func TestRuntimeJournalRejectsSharedDirectoryAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and symlinks")
	}
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := SaveRuntimeJournal(filepath.Join(shared, "checkpoint.pb"), &Config{}); err == nil {
		t.Fatal("shared checkpoint directory was accepted")
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "checkpoint.pb")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("do not overwrite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := SaveRuntimeJournal(link, &Config{}); err == nil {
		t.Fatal("symlink checkpoint was overwritten")
	}
	if _, err := LoadRuntimeJournal(link); err == nil {
		t.Fatal("symlink checkpoint was loaded")
	}
	content, _ := os.ReadFile(target)
	if string(content) != "do not overwrite" {
		t.Fatal("unrelated file was modified")
	}
}
