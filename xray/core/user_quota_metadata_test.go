package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestUserQuotaRequirementIsPartOfCachedCredentialsAndSurvivesOlderProfileUpdates(t *testing.T) {
	config := &Config{}
	if required, err := ConfigRequiresUserQuota(config); err != nil || required {
		t.Fatal("plain configuration acquired a quota requirement", err)
	}
	for range 2 {
		if err := RequireUserQuotaConfig(config); err != nil {
			t.Fatal(err)
		}
	}
	if len(config.App) != 1 {
		t.Fatal("quota metadata duplicated")
	}
	server, err := New(config)
	if err != nil {
		t.Fatal("server-only metadata was treated as an unknown runtime feature", err)
	}
	t.Cleanup(func() { server.Close() })
	if !server.RequiresUserQuota() {
		t.Fatal("cached quota requirement was ignored")
	}
	scope := NewFeatureScope(server)
	t.Cleanup(func() { scope.Close() })
	if !scope.Instance.RequiresUserQuota() {
		t.Fatal("a native generation dropped the user quota requirement")
	}
	server.RecordConfiguration(&Config{})
	if required, err := ConfigRequiresUserQuota(server.Configuration()); err != nil || !required {
		t.Fatal("an older profile removed the recorded quota requirement", err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(root, "checkpoint.pb")
	server.SetRuntimeJournal(checkpoint)
	if err := server.SaveRuntimeConfiguration(&Config{}); err != nil {
		t.Fatal(err)
	}
	data, readErr := os.ReadFile(checkpoint)
	if readErr != nil || !strings.HasPrefix(string(data), runtimeUserQuotaJournalMagic) {
		t.Fatal("user quota checkpoint is not distinguishable before image selection", readErr)
	}
	cached, err := LoadRuntimeJournal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if required, err := ConfigRequiresUserQuota(cached); err != nil || !required {
		t.Fatal("restart checkpoint forgot the quota requirement", err)
	}
	restarted, err := New(cached)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !restarted.RequiresUserQuota() {
		t.Fatal("restarted native credentials lost quota protection")
	}
	forged := strings.Replace(string(data), runtimeUserQuotaJournalMagic, runtimeJournalMagic, 1)
	if err := os.WriteFile(checkpoint, []byte(forged), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeJournal(checkpoint); err == nil {
		t.Fatal("a header change hid an existing quota requirement")
	}
}

func TestUserQuotaMalformedDuplicateAndFutureRequirementMetadataFailClosed(t *testing.T) {
	for _, fields := range []map[string]any{
		{"version": "future", "userQuota": true}, {"version": "remnacust-core-requirements-v1", "userQuota": false},
		{"version": "remnacust-core-requirements-v1", "userQuota": "true"}, {"version": "remnacust-core-requirements-v1", "userQuota": true, "unknown": true},
	} {
		value, _ := structpb.NewStruct(fields)
		data, _ := proto.Marshal(value)
		config := &Config{App: []*serial.TypedMessage{{Type: UserQuotaRequirementType, Value: data}}}
		if _, err := ConfigRequiresUserQuota(config); err == nil {
			t.Fatal("invalid metadata accepted")
		}
		if _, err := New(config); err == nil {
			t.Fatal("invalid metadata started a core")
		}
	}
	config := &Config{}
	if err := RequireUserQuotaConfig(config); err != nil {
		t.Fatal(err)
	}
	config.App = append(config.App, proto.Clone(config.App[0]).(*serial.TypedMessage))
	if _, err := New(config); err == nil {
		t.Fatal("duplicate requirement records accepted")
	}
}

func TestUserQuotaNativeChildInheritsRequirementCommittedAfterItsCreation(t *testing.T) {
	root := &Instance{ctx: context.Background()}
	scope := NewFeatureScope(root)
	if scope.Instance.RequiresUserQuota() {
		t.Fatal("plain generation acquired a quota requirement")
	}
	config := &Config{}
	if err := RequireUserQuotaConfig(config); err != nil {
		t.Fatal(err)
	}
	root.RecordConfiguration(config)
	if !scope.Instance.RequiresUserQuota() {
		t.Fatal("an existing native generation escaped the committed quota requirement")
	}
}
