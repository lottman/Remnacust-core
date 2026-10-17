//go:build !wasm

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xtls/xray-core/core"
	"google.golang.org/protobuf/proto"
)

func profileInputFixture(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"version":  profileInputVersion,
		"root":     map[string]any{"log": map[string]any{"loglevel": "none"}, "outbounds": []any{map[string]any{"tag": "root", "protocol": "freedom"}}},
		"delivery": map[string]any{"epoch": "2", "owner": "11111111-1111-4111-8111-111111111111", "revision": "1", "enabled": true, "inputsHash": strings.Repeat("a", 64)},
		"graphs":   []any{map[string]any{"bindingUuid": "11111111-1111-4111-8111-111111111111", "configProfileUuid": "22222222-2222-4222-8222-222222222222", "configProfileInboundUuid": "33333333-3333-4333-8333-333333333333", "config": map[string]any{"outbounds": []any{map[string]any{"tag": "direct", "protocol": "freedom"}}}}},
	}
}

func profileInputJSON(t *testing.T, input map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestProfileInputCompilesNativeSourceAndPreservesCacheReplay(t *testing.T) {
	input := profileInputFixture(t)
	data := profileInputJSON(t, input)
	config, err := compileProfileInput(data, true)
	if err != nil {
		t.Fatal(err)
	}
	clock, err := core.ConfigProfileDelivery(config)
	if err != nil || clock == nil {
		t.Fatal(clock, err)
	}
	digest, err := core.ProfilePlanDigest(config)
	if err != nil || digest != clock.PlanHash {
		t.Fatal("native source digest was not computed", err)
	}
	file := filepath.Join(t.TempDir(), "state", "plan.pb")
	ack, err := prepareProfileCheckpoint(file, data)
	if err != nil || ack["graphCount"] != 1 {
		t.Fatal(ack, err)
	}
	saved, err := core.LoadRuntimeJournal(file)
	if err != nil {
		t.Fatal(err)
	}
	bindings, _, _ := core.ConfigProfileBindings(saved)
	if err := core.RequireUserQuotaConfig(bindings[0].Config); err != nil {
		t.Fatal(err)
	}
	saved, err = core.WithProfileBindings(saved, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SaveRuntimeJournal(file, saved); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	ack, err = prepareProfileCheckpoint(file, data)
	if err != nil || ack["unchanged"] != true {
		t.Fatal("exact cache replay rewrote imperative policy", ack, err)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("cache replay changed the existing checkpoint")
	}
	input["delivery"].(map[string]any)["epoch"] = "3"
	if _, err := prepareProfileCheckpoint(file, profileInputJSON(t, input)); err != nil {
		t.Fatal(err)
	}
	retained, err := core.LoadRuntimeJournal(file)
	if err != nil {
		t.Fatal(err)
	}
	retainedBindings, _, _ := core.ConfigProfileBindings(retained)
	if required, err := core.ConfigRequiresUserQuota(retainedBindings[0].Config); err != nil || !required {
		t.Fatal("new delivery disabled an enrolled graph's mandatory quota", err)
	}
	input["delivery"].(map[string]any)["epoch"] = "1"
	if _, err := prepareProfileCheckpoint(file, profileInputJSON(t, input)); err == nil {
		t.Fatal("old delivery overwrote the checkpoint")
	}
	final, _ := core.LoadRuntimeJournal(file)
	if !proto.Equal(final, retained) {
		t.Fatal("rejected cache delivery changed disk")
	}
}

func TestProfileInputRejectsAmbiguousEnvironmentBeforeBuild(t *testing.T) {
	const key = "REMNACUST_PROFILE_ENV_FIXTURE"
	t.Setenv(key, "unchanged")
	for _, field := range []string{"env", "ENV", "eNv"} {
		input := profileInputFixture(t)
		input["graphs"].([]any)[0].(map[string]any)["config"].(map[string]any)[field] = map[string]any{key: "changed"}
		if _, err := compileProfileInput(profileInputJSON(t, input), false); err == nil {
			t.Fatal("profile environment accepted", field)
		}
		if os.Getenv(key) != "unchanged" {
			t.Fatal("failed validation changed process environment")
		}
	}
	input := profileInputFixture(t)
	raw := profileInputJSON(t, input)
	raw = bytes.Replace(raw, []byte(`"root":{`), []byte(`"root":{"env":{"`+key+`":"changed"},"env":{},`), 1)
	if _, err := compileProfileInput(raw, false); err == nil {
		t.Fatal("duplicate environment merge accepted")
	}
	if os.Getenv(key) != "unchanged" {
		t.Fatal("duplicate fields reached the JSON builder")
	}
	for name, mutate := range map[string]func(map[string]any){
		"wrong version":      func(v map[string]any) { v["version"] = "other" },
		"unknown field":      func(v map[string]any) { v["unexpected"] = true },
		"missing enabled":    func(v map[string]any) { delete(v["delivery"].(map[string]any), "enabled") },
		"disabled exposure":  func(v map[string]any) { v["delivery"].(map[string]any)["enabled"] = false },
		"invalid provenance": func(v map[string]any) { v["graphs"].([]any)[0].(map[string]any)["bindingUuid"] = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			v := profileInputFixture(t)
			mutate(v)
			if _, err := compileProfileInput(profileInputJSON(t, v), false); err == nil {
				t.Fatal("invalid profile input accepted")
			}
		})
	}
}

func TestRuntimeControlCommandNamesReachTheDispatcher(t *testing.T) {
	wanted := map[string]bool{"reconcile": false, "cache-runtime": false, "require-user-quota": false, "inspect-checkpoint": false, "usage-receipt": false, "profile-inspect": false, "profile-apply": false, "cache-profiles": false}
	seen := make(map[string]bool)
	for _, cmd := range CmdAPI.Commands {
		name := cmd.Name()
		if seen[name] {
			t.Fatalf("duplicate CLI route %q", name)
		}
		seen[name] = true
		if _, ok := wanted[name]; ok {
			wanted[name] = true
		}
	}
	for name, present := range wanted {
		if !present {
			t.Errorf("CLI command %s cannot be selected by the actual dispatcher", name)
		}
	}
}
