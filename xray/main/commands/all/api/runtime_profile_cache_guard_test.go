//go:build !wasm

package api

import (
	"testing"

	"github.com/xtls/xray-core/core"
)

func TestLegacyRuntimeCacheCannotEraseManagedProfilePlan(t *testing.T) {
	if err := validateLegacyRuntimeCache(&core.Config{}); err != nil {
		t.Fatal("legacy checkpoint compatibility changed", err)
	}
	plan, err := core.WithProfileBindings(&core.Config{}, []core.ProfileBinding{{
		BindingID: "11111111-1111-4111-8111-111111111111", ProfileID: "22222222-2222-4222-8222-222222222222", InboundID: "33333333-3333-4333-8333-333333333333", Config: &core.Config{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLegacyRuntimeCache(plan); err == nil {
		t.Fatal("legacy restart could silently erase independent graphs and their delivery clock")
	}
}
