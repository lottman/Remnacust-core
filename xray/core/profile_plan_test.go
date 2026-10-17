package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/stats"
	"google.golang.org/protobuf/proto"
)

const planBindingA = "11111111-1111-4111-8111-111111111111"
const planBindingB = "22222222-2222-4222-8222-222222222222"
const planProfileID = "33333333-3333-4333-8333-333333333333"
const planInboundID = "44444444-4444-4444-8444-444444444444"

func testProfilePlan(t *testing.T) *Config {
	t.Helper()
	config, err := WithProfileBindings(&Config{}, []ProfileBinding{
		{BindingID: planBindingB, ProfileID: planProfileID, InboundID: planInboundID, Config: &Config{}},
		{BindingID: planBindingA, ProfileID: planProfileID, InboundID: planInboundID, Config: &Config{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestProfilePlanProvenanceAndEncoding(t *testing.T) {
	config := testProfilePlan(t)
	bindings, found, err := ConfigProfileBindings(config)
	if err != nil || !found || len(bindings) != 2 || bindings[0].BindingID != planBindingA {
		t.Fatal(bindings, found, err)
	}
	reencoded, err := WithProfileBindings(config, bindings)
	if err != nil || !proto.Equal(reencoded, config) {
		t.Fatal("profile encoding is not deterministic", err)
	}
	bindings[0].Config.Inbound = append(bindings[0].Config.Inbound, &InboundHandlerConfig{Tag: "mutated"})
	parsed, _, _ := ConfigProfileBindings(config)
	if len(parsed[0].Config.Inbound) != 0 {
		t.Fatal("decoded graph aliases source metadata")
	}
	for name, invalid := range map[string][]ProfileBinding{
		"duplicate":    {parsed[0], parsed[0]},
		"invalid UUID": {{BindingID: "bad", ProfileID: planProfileID, InboundID: planInboundID, Config: &Config{}}},
		"nested":       {{BindingID: planBindingA, ProfileID: planProfileID, InboundID: planInboundID, Config: config}},
		"nil inbound":  {{BindingID: planBindingA, ProfileID: planProfileID, InboundID: planInboundID, Config: &Config{Inbound: []*InboundHandlerConfig{nil}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := WithProfileBindings(&Config{}, invalid); err == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
	invalid := proto.Clone(config).(*Config)
	invalid.App = append(invalid.App, proto.Clone(invalid.App[0]).(*serial.TypedMessage))
	if _, _, err := ConfigProfileBindings(invalid); err == nil {
		t.Fatal("duplicate metadata accepted")
	}
	file := filepath.Join(t.TempDir(), "state", "plan.pb")
	if err := SaveRuntimeJournal(file, invalid); err == nil {
		t.Fatal("invalid plan was persisted")
	}
}

func profilePlanTestOwner(t *testing.T, config *Config, path string) *Instance {
	t.Helper()
	owner, err := NewProfile(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	// This core-package lifecycle fixture has no application registrations.
	for i, feature := range owner.features {
		if feature.Type() == stats.ManagerType() {
			owner.features[i] = new(managedProfileAccounting)
		}
	}
	owner.SetRuntimeJournal(path)
	t.Cleanup(func() { owner.Close() })
	return owner
}

func TestProfilePlanStartsAndPersistsEveryManagedGraph(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "plan.pb")
	owner := profilePlanTestOwner(t, testProfilePlan(t), file)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	a, b := owner.ManagedProfile(planBindingA), owner.ManagedProfile(planBindingB)
	if a == nil || b == nil || !a.IsRunning() || !b.IsRunning() || owner.RuntimeAdmissionFenced() || !owner.RuntimeManagementReady() {
		t.Fatal("plan did not become ready together")
	}
	if a.RuntimeAccess() == b.RuntimeAccess() || a.ManagementAccess() != b.ManagementAccess() || a.GetFeature(stats.ManagerType()) != b.GetFeature(stats.ManagerType()) {
		t.Fatal("profile isolation/accounting/management ownership is incorrect")
	}
	if !a.RuntimeCheckpointEnabled() {
		t.Fatal("managed graph did not inherit manifest durability")
	}
	changed := &Config{Inbound: []*InboundHandlerConfig{}}
	if err := RequireUserQuotaConfig(changed); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveRuntimeConfiguration(changed); err != nil {
		t.Fatal(err)
	}
	a.RecordConfiguration(changed)
	saved, err := LoadRuntimeJournal(file)
	if err != nil {
		t.Fatal(err)
	}
	bindings, _, err := ConfigProfileBindings(saved)
	if err != nil || !proto.Equal(bindings[0].Config, changed) || len(bindings[1].Config.App) != 0 {
		t.Fatal("single-graph mutation did not preserve the complete manifest", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := profilePlanTestOwner(t, saved, file)
	if err := restarted.Start(); err != nil {
		t.Fatal(err)
	}
	if !restarted.ManagedProfile(planBindingA).RequiresUserQuota() || restarted.ManagedProfile(planBindingB).RequiresUserQuota() {
		t.Fatal("restart changed graph quota requirements")
	}
}

func TestProfilePlanRefusesMissingCheckpointAndFencesUncertainStore(t *testing.T) {
	owner := profilePlanTestOwner(t, testProfilePlan(t), "")
	if err := owner.Start(); err == nil || owner.ManagedProfile(planBindingA) != nil {
		t.Fatal("undurable plan started")
	}
	file := filepath.Join(t.TempDir(), "state", "plan.pb")
	owner = profilePlanTestOwner(t, testProfilePlan(t), file)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	// Replacing the trusted target with a directory makes both commit and
	// restoration fail; no unverified manifest can reopen profile admission.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	if err := owner.ManagedProfile(planBindingA).SaveRuntimeConfiguration(&Config{}); err == nil {
		t.Fatal("uncertain store reported success")
	}
	if !owner.RuntimeAdmissionFenced() || !owner.ManagedProfile(planBindingB).RuntimeAdmissionFenced() || owner.RuntimeManagementReady() {
		t.Fatal("uncertain whole-plan outcome did not fence sibling admission")
	}
}

func TestProfilePlanStartupRetainsDurableSnapshotWithInheritedQuota(t *testing.T) {
	config := testProfilePlan(t)
	if err := RequireUserQuotaConfig(config); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "state", "plan.pb")
	owner := profilePlanTestOwner(t, config, file)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadRuntimeJournal(file)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := ConfigProfileBindings(config)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := ConfigProfileBindings(saved)
	if err != nil || len(after) != len(before) {
		t.Fatal("startup changed the saved graph roster", err)
	}
	for i := range before {
		if !proto.Equal(before[i].Config, after[i].Config) {
			t.Fatal("child initialization rewrote its already durable graph snapshot")
		}
	}
	if required, err := ConfigRequiresUserQuota(saved); err != nil || !required {
		t.Fatal("startup lost the durable root quota requirement", err)
	}
	for _, id := range []string{planBindingA, planBindingB} {
		if !owner.ManagedProfile(id).RequiresUserQuota() {
			t.Fatal("child startup lost inherited mandatory quota")
		}
	}
}
