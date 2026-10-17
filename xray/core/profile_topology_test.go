package core

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestProfileTopologyRejectsAliasedPreparedGraphBeforeAdmissionLocks(t *testing.T) {
	owner := profilePlanTestOwner(t, testProfilePlan(t), filepath.Join(t.TempDir(), "state", "plan.pb"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	staged, err := NewStagedManagedProfile(owner, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if err := staged.Start(); err != nil {
		t.Fatal(err)
	}
	target := owner.Configuration()
	digest, err := ProfilePlanDigest(target)
	if err != nil {
		t.Fatal(err)
	}
	target, err = WithProfileDelivery(target, ProfileDelivery{Epoch: "1", Owner: planBindingA, Revision: "1", Enabled: true, InputsHash: strings.Repeat("a", 64), PlanHash: digest})
	if err != nil {
		t.Fatal(err)
	}
	before := owner.Configuration()
	owner.ManagementAccess().Lock()
	err = owner.CommitManagedProfileTopology(target, map[string]*Instance{planBindingA: staged, planBindingB: staged})
	owner.ManagementAccess().Unlock()
	if err == nil {
		t.Fatal("one graph was enrolled twice under different binding identities")
	}
	if !proto.Equal(before, owner.Configuration()) {
		t.Fatal("rejected alias changed the owning checkpoint")
	}
}
