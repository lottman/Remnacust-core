package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/policy"
)

func TestProfileInheritsMandatoryQuotaWithoutBorrowingLifecycle(t *testing.T) {
	for _, requireBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled-later", true: "already-required"}[requireBefore], func(t *testing.T) {
			root, err := New(&Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			requirement := &Config{}
			if err := RequireUserQuotaConfig(requirement); err != nil {
				t.Fatal(err)
			}
			if requireBefore {
				root.RecordConfiguration(requirement)
			}
			input := &Config{}
			profile, err := NewProfile(root.RuntimeContext(), input)
			if err != nil {
				t.Fatal(err)
			}
			defer profile.Close()
			if len(input.App) != 0 {
				t.Fatal("profile constructor modified caller configuration")
			}
			if profile.RequiresUserQuota() != requireBefore {
				t.Fatal("incorrect initial quota requirement")
			}
			if !requireBefore {
				root.RecordConfiguration(requirement)
			}
			if !profile.RequiresUserQuota() {
				t.Fatal("profile escaped parent quota requirement")
			}
			if required, err := ConfigRequiresUserQuota(profile.Configuration()); err != nil || !required {
				t.Fatal("profile snapshot dropped an inherited quota requirement", err)
			}
			generation := NewFeatureScope(profile)
			defer generation.Close()
			if !generation.Instance.RequiresUserQuota() {
				t.Fatal("profile generation escaped quota requirement")
			}
			profile.RecordConfiguration(&Config{})
			if required, err := ConfigRequiresUserQuota(profile.Configuration()); err != nil || !required {
				t.Fatal("profile update dropped quota metadata", err)
			}
			checkpoint := filepath.Join(t.TempDir(), "private", "profile.pb")
			profile.SetRuntimeJournal(checkpoint)
			if err := profile.SaveRuntimeConfiguration(&Config{}); err != nil {
				t.Fatal(err)
			}
			cached, err := LoadRuntimeJournal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := NewProfile(context.Background(), cached)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if !restarted.RequiresUserQuota() {
				t.Fatal("autonomous restart lost mandatory quota")
			}
			if profile.RuntimeAccess() == root.RuntimeAccess() {
				t.Fatal("independent profile borrowed parent admission lock")
			}
			if err := profile.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-root.RuntimeDone():
				t.Fatal("closing profile closed parent")
			default:
			}
		})
	}
}

func TestProfileConstructionDoesNotReloadProcessEnvironment(t *testing.T) {
	// Register this before Setenv: environment restoration must happen before
	// restoring the package defaults used by the other tests.
	t.Cleanup(func() {
		if err := platform.ReloadEnvSettings(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv(platform.BufferSize, "16")
	root, err := New(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before := policy.SessionDefault().Buffer.PerConnection
	if before != 16*1024*1024 {
		t.Fatalf("unexpected root environment policy: %d", before)
	}
	t.Setenv(platform.BufferSize, "32")
	profile, err := NewProfile(context.Background(), &Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	if policy.SessionDefault().Buffer.PerConnection != before {
		t.Fatal("profile creation changed process policy defaults")
	}
	legacy, err := New(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if policy.SessionDefault().Buffer.PerConnection != 32*1024*1024 {
		t.Fatal("legacy root initialization no longer reloads environment")
	}
}

func TestProfileConstructionRejectsMissingAndUnknownConfiguration(t *testing.T) {
	if _, err := NewProfile(nil, &Config{}); err == nil {
		t.Fatal("nil profile context accepted")
	}
	if _, err := NewProfile(context.Background(), nil); err == nil {
		t.Fatal("nil profile config accepted")
	}
	if _, err := New(nil); err == nil {
		t.Fatal("nil root config accepted")
	}
	if _, err := NewProfile(context.Background(), &Config{App: []*serial.TypedMessage{{Type: "unsupported.profile.feature"}}}); err == nil {
		t.Fatal("unknown profile feature was silently omitted")
	}
}
