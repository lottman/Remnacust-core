package core

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"google.golang.org/protobuf/proto"
)

func TestProfileDeliveryClockSurvivesCheckpointAndImperativeMutation(t *testing.T) {
	config := testProfilePlan(t)
	bindings, _, _ := ConfigProfileBindings(config)
	digest, err := ProfileBindingsDigest(bindings)
	if err != nil {
		t.Fatal(err)
	}
	clock := ProfileDelivery{Epoch: "9223372036854775807", Owner: planBindingA, Revision: "19", InputsHash: strings.Repeat("a", 64), PlanHash: digest, Enabled: true}
	config, err = WithProfileDelivery(config, clock)
	if err != nil {
		t.Fatal(err)
	}
	disabled := clock
	disabled.Enabled = false
	if _, err := WithProfileDelivery(config, disabled); err == nil {
		t.Fatal("disabled delivery retained exposed graphs")
	}
	if err := RequireUserQuotaConfig(config); err != nil {
		t.Fatal(err)
	}
	bindings[0].Config = proto.Clone(bindings[0].Config).(*Config)
	if err := RequireUserQuotaConfig(bindings[0].Config); err != nil {
		t.Fatal(err)
	}
	withPolicy, err := ProfileBindingsDigest(bindings)
	if err != nil || digest != withPolicy {
		t.Fatal("server policy changed source delivery identity", err)
	}
	fullSource, err := ProfilePlanDigest(config)
	if err != nil {
		t.Fatal(err)
	}
	changedRoot := proto.Clone(config).(*Config)
	changedRoot.Extension = append(changedRoot.Extension, &serial.TypedMessage{Type: "test.source-control-extension", Value: []byte("changed")})
	changedSource, err := ProfilePlanDigest(changedRoot)
	if err != nil || changedSource == fullSource {
		t.Fatal("source identity omitted root control settings", err)
	}
	config, err = WithProfileBindings(config, bindings)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "state", "clock.pb")
	if err := SaveRuntimeJournal(file, config); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadRuntimeJournal(file)
	if err != nil || !proto.Equal(saved, config) {
		t.Fatal("clocked checkpoint changed", err)
	}
	actual, err := ConfigProfileDelivery(saved)
	if err != nil || actual == nil || *actual != clock {
		t.Fatal("clock precision or source identity lost", actual, err)
	}
	duplicate := proto.Clone(saved).(*Config)
	for _, app := range saved.App {
		if app.Type == ProfileDeliveryType {
			duplicate.App = append(duplicate.App, app)
			break
		}
	}
	if _, _, err := ConfigProfileBindings(duplicate); err == nil {
		t.Fatal("duplicate delivery metadata accepted")
	}
}

func TestProfileDeliveryClockRejectsRollbackAndLeaseReuse(t *testing.T) {
	previous := ProfileDelivery{Epoch: "20", Owner: planBindingA, Revision: "3", InputsHash: strings.Repeat("a", 64), PlanHash: strings.Repeat("b", 64), Enabled: true}
	if err := CheckProfileDeliveryAdvance(&previous, &previous); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ProfileDelivery){
		"old epoch":                   func(d *ProfileDelivery) { d.Epoch = "19" },
		"old revision with new epoch": func(d *ProfileDelivery) { d.Epoch = "21"; d.Revision = "2" },
		"owner reuse":                 func(d *ProfileDelivery) { d.Owner = planBindingB },
		"source hash reuse":           func(d *ProfileDelivery) { d.PlanHash = strings.Repeat("c", 64) },
		"input hash reuse":            func(d *ProfileDelivery) { d.InputsHash = strings.Repeat("c", 64) },
		"same revision mode change":   func(d *ProfileDelivery) { d.Epoch = "21"; d.Enabled = false },
		"overflow":                    func(d *ProfileDelivery) { d.Epoch = "9223372036854775808" },
		"noncanonical decimal":        func(d *ProfileDelivery) { d.Epoch = "020" },
	} {
		t.Run(name, func(t *testing.T) {
			target := previous
			mutate(&target)
			if err := CheckProfileDeliveryAdvance(&previous, &target); err == nil {
				t.Fatal("conflicting delivery accepted")
			}
		})
	}
	if err := CheckProfileDeliveryAdvance(&previous, nil); err == nil {
		t.Fatal("clock downgrade accepted")
	}
	next := previous
	next.Epoch = "21"
	next.Owner = planBindingB
	next.Revision = "4"
	next.Enabled = false
	if err := CheckProfileDeliveryAdvance(&previous, &next); err != nil {
		t.Fatal(err)
	}
}
