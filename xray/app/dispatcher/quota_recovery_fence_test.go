//go:build !wasm

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRecoveryHeldPolicyKeepsHostLegacyDeviceAndExemptStreamsPausedAfterGlobalGate(t *testing.T) {
	recoveryTestState(t)
	policy := userQuotaFixture(t, "100", userQuotaScale)
	access := policy.Hosts[hostB]
	access.UserQuotaExempt, access.Groups = true, []string{"b"}
	policy.Hosts[hostB] = access
	if !publishHostPolicy(policy) {
		t.Fatal("initial policy rejected")
	}
	before := make(map[string]quotaEntry)
	for id, entry := range localQuota.entries {
		before[id] = entry
	}
	fence := recoveryRecord(nil, uuid.NewString(), "FENCED")
	if err := publishRecoveryFence(fence, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(policyContext("shared"), 2*time.Second)
	defer cancel()
	var continued atomic.Int64
	done := make(chan error, 1)
	go func() {
		done <- waitHostPolicyAndAccount(ctx, hostEmail("1", hostA), "up", 1, func() { continued.Add(1) })
	}()
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	var held hostPolicyDocument
	if err := json.Unmarshal(data, &held); err != nil {
		t.Fatal(err)
	}
	held.Generation = "recovery-staged-hold"
	held.Registrations = nil
	for key, group := range held.Groups {
		group.FenceAll, group.Reserves = true, nil
		held.Groups[key] = group
	}
	if !publishHostPolicy(held) {
		t.Fatal("held recovery policy rejected")
	}
	if err := publishRecoveryFence(recoveryRecord(fence, fence.Generation, "RELEASED"), false); err != nil {
		t.Fatal(err)
	}
	var accounted atomic.Int64
	for _, sample := range []struct{ email, tag string }{
		{hostEmail("1", hostA), "shared"}, {hostEmail("1", hostB), "shared"},
		{"1", "ordinary"}, {"1~0123456789abcdef01234567", "ordinary"}, {"2", "ordinary"},
	} {
		err := admitHostPolicy(policyContext(sample.tag), sample.email, "up", 1, func() { accounted.Add(1) })
		if !errors.Is(err, errHostPolicyFenced) {
			t.Fatalf("%s escaped the held policy: %v", sample.email, err)
		}
	}
	if accounted.Load() != 0 {
		t.Fatal("paused streams were charged")
	}
	for id, old := range before {
		entry, exists := localQuota.entries[id]
		if !exists || entry.Ceiling != old.Ceiling || entry.Reserved != old.Reserved || entry.consumed != old.consumed {
			t.Fatal("staging discarded or spent a previous reserve")
		}
	}
	select {
	case err := <-done:
		t.Fatal("staging or global release interrupted the waiting stream", err)
	case <-time.After(20 * time.Millisecond):
	}
	var resumed hostPolicyDocument
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	resumed.Generation = "recovery-confirmed-resume"
	group := resumed.Groups["user:global"]
	group.Reserves = nil
	group.UnlimitedOwners = map[string]bool{"1": true, "2": true}
	resumed.Groups["user:global"] = group
	if !publishHostPolicy(resumed) {
		t.Fatal("final policy rejected")
	}
	if err := <-done; err != nil || continued.Load() != 1 {
		t.Fatal("waiting stream did not continue exactly once", err, continued.Load())
	}
}

func recoveryTestState(t *testing.T) {
	t.Helper()
	hostPolicies.Lock()
	old, invalid := hostPolicies.recoveryFence, hostPolicies.recoveryInvalid
	hostPolicies.recoveryFence, hostPolicies.recoveryInvalid = nil, false
	hostPolicies.Unlock()
	t.Cleanup(func() {
		hostPolicies.Lock()
		hostPolicies.recoveryFence, hostPolicies.recoveryInvalid = old, invalid
		notifyHostPolicyLocked()
		hostPolicies.Unlock()
	})
}

func recoveryRecord(previous *quotaRecoveryFence, generation, state string) *quotaRecoveryFence {
	value := &quotaRecoveryFence{Version: quotaRecoveryFenceCapability, Generation: generation,
		Token: uuid.NewString(), State: state, Sequence: "1"}
	if previous != nil {
		value.Sequence, value.Previous = nextRecoverySequence(previous.Sequence), &previous.Digest
	}
	value.Digest = recoveryFenceDigest(*value)
	return value
}

func TestQuotaRecoveryFenceProtocolDigest(t *testing.T) {
	value := quotaRecoveryFence{Version: quotaRecoveryFenceCapability, Generation: "11111111-1111-4111-8111-111111111111",
		Token: "22222222-2222-4222-8222-222222222222", State: "FENCED", Sequence: "1",
		Digest: "a956d69b330b83f9d303c91064f5f9cbb7728c1df810bbf11a122dcc52e0fc8b"}
	if !validRecoveryFence(value) {
		t.Fatal("node/core recovery fence digest contract differs")
	}
}

func TestQuotaRecoveryFenceKeepsConnectionsAndRequiresExplicitMatchingRelease(t *testing.T) {
	recoveryTestState(t)
	testHostPolicy(t, v2Policy())
	fence := recoveryRecord(nil, uuid.NewString(), "FENCED")
	if err := publishRecoveryFence(fence, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(policyContext("shared"), 2*time.Second)
	defer cancel()
	var accounted atomic.Int64
	done := make(chan error, 1)
	go func() {
		done <- waitHostPolicyAndAccount(ctx, hostEmail("1", hostA), "up", 1, func() { accounted.Add(1) })
	}()
	select {
	case err := <-done:
		t.Fatal("recovery pause closed a connection", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := waitHostPolicy(context.Background(), "internal-api", "up", 1); err != nil {
		t.Fatal("recovery pause affected a non-subscriber API link", err)
	}
	next := v2Policy()
	next.Generation = "ordinary-update-cannot-resume"
	if !publishHostPolicy(next) {
		t.Fatal("fixture policy was rejected")
	}
	if err := publishRecoveryFence(nil, false); err == nil {
		t.Fatal("missing marker released a recovery fence")
	}
	release := recoveryRecord(fence, uuid.NewString(), "RELEASED")
	if err := publishRecoveryFence(release, false); err == nil {
		t.Fatal("a different restore generation released traffic")
	}
	if accounted.Load() != 0 {
		t.Fatal("paused bytes were counted")
	}
	release = recoveryRecord(fence, fence.Generation, "RELEASED")
	if err := publishRecoveryFence(release, false); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || accounted.Load() != 1 {
		t.Fatal("existing connection did not resume exactly once", err, accounted.Load())
	}
	if err := publishRecoveryFence(fence, false); err == nil {
		t.Fatal("an old marker was replayed")
	}
}

func TestQuotaRecoveryFenceWaitsForAlreadyAdmittedAccounting(t *testing.T) {
	recoveryTestState(t)
	testHostPolicy(t, v2Policy())
	hostPolicies.Lock()
	hostPolicies.admissions["host:"+hostA]++
	ticket := &hostAdmissionTicket{key: "host:" + hostA, pending: true}
	hostPolicies.Unlock()
	done := make(chan error, 1)
	go func() { done <- publishRecoveryFence(recoveryRecord(nil, uuid.NewString(), "FENCED"), false) }()
	defer ticket.finish()
	select {
	case err := <-done:
		t.Fatal("ACK escaped before old admission finished", err)
	case <-time.After(20 * time.Millisecond):
	}
	ticket.finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("accounting completion did not release the fence ACK")
	}
}

func TestQuotaRecoveryFenceSurvivesRestartAndRejectsDamagedPrivateState(t *testing.T) {
	recoveryTestState(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	marker, status := filepath.Join(directory, "marker.json"), filepath.Join(directory, "status.json")
	fence := recoveryRecord(nil, uuid.NewString(), "FENCED")
	data, _ := json.Marshal(fence)
	if err := os.WriteFile(marker, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "recovery-required.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readRecoveryFence(marker); err == nil {
			t.Fatal("public recovery directory was accepted")
		}
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := refreshRecoveryFence(marker, status, true); err != nil {
		t.Fatal(err)
	}
	hostPolicies.Lock()
	paused := hostAdmissionFenced("12", nil)
	hostPolicies.recoveryFence = nil // a new process starts without previous memory
	hostPolicies.Unlock()
	if !paused {
		t.Fatal("startup did not pause a raw managed identity")
	}
	if err := refreshRecoveryFence(marker, status, true); err != nil {
		t.Fatal(err)
	}
	ack, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	var proof struct {
		PID    int    `json:"pid"`
		Core   string `json:"coreProcessId"`
		Digest string `json:"digest"`
		State  string `json:"state"`
	}
	if json.Unmarshal(ack, &proof) != nil || proof.PID != os.Getpid() || proof.Core != coreIncarnation || proof.Digest != fence.Digest || proof.State != "FENCED" {
		t.Fatal("fence ACK is not bound to the live process")
	}
	if err := os.WriteFile(marker, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := refreshRecoveryFence(marker, status, false); err != nil {
		t.Fatal("corrupt mutable record did not recover the restrictive first fence", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := refreshRecoveryFence(marker, status, false); err != nil {
		t.Fatal("missing mutable record did not retain the restrictive first fence", err)
	}
	hostPolicies.Lock()
	hostPolicies.recoveryFence = nil
	hostPolicies.Unlock()
	if err := refreshRecoveryFence(marker, status, true); err != nil {
		t.Fatal("cold start lost the immutable recovery pause", err)
	}
	hostPolicies.Lock()
	paused = hostAdmissionFenced("12", nil)
	hostPolicies.Unlock()
	if !paused {
		t.Fatal("corrupt or missing marker did not remain paused")
	}
	if err := os.WriteFile(filepath.Join(directory, "recovery-required.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if refreshRecoveryFence(marker, status, false) == nil {
		t.Fatal("corrupt immutable requirement was accepted")
	}
}
