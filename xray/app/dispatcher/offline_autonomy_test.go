//go:build !wasm

package dispatcher

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

func offlineUserQuotaPolicy(t *testing.T, ceiling int64, grantLifetime, identityLifetime time.Duration) (hostPolicyDocument, func()) {
	t.Helper()
	previousQuota := localQuota
	hostPolicies.Lock()
	previousDocument, previousBuckets := hostPolicies.document, hostPolicies.buckets
	previousFrozen, previousAdmissions := hostPolicies.frozen, hostPolicies.admissions
	previousChanged, previousDrain := hostPolicies.changed, hostPolicies.pendingDrain
	previousFence, previousInvalid := hostPolicies.recoveryFence, hostPolicies.recoveryInvalid
	hostPolicies.document = hostPolicyDocument{}
	hostPolicies.buckets = make(map[string]*hostBucket)
	hostPolicies.frozen = make(map[string]bool)
	hostPolicies.admissions = make(map[string]int)
	hostPolicies.changed = make(chan struct{})
	hostPolicies.pendingDrain = 0
	hostPolicies.recoveryFence, hostPolicies.recoveryInvalid = nil, false
	hostPolicies.Unlock()
	restore := func() {
		localQuota = previousQuota
		hostPolicies.Lock()
		hostPolicies.document, hostPolicies.buckets = previousDocument, previousBuckets
		hostPolicies.frozen, hostPolicies.admissions = previousFrozen, previousAdmissions
		hostPolicies.changed, hostPolicies.pendingDrain = previousChanged, previousDrain
		hostPolicies.recoveryFence, hostPolicies.recoveryInvalid = previousFence, previousInvalid
		hostPolicies.Unlock()
	}
	t.Cleanup(restore)
	localQuota = quotaTestStore(t)
	debit := quotaTestDebit("aaaaaaaa-1111-4111-8111-111111111111", ceiling, 0)
	debit.Group, debit.Owner, debit.Scale = "user:global", "1", userQuotaScale
	debit.Grant.ExpiresAt = time.Now().Add(grantLifetime).UnixMilli()
	debit.Grant.RegistrationRequired = true
	if err := localQuota.reconcile(registrationPolicy(debit, "prepare"), time.Now()); err != nil {
		t.Fatal(err)
	}
	policy := v2Policy()
	policy.Generation = "offline-user-quota"
	policy.ExpiresAt = time.Now().Add(90 * time.Second).UnixMilli()
	policy.OfflineReserves, policy.UserQuota = quotaReserveCapability, userQuotaCapability
	policy.QuotaRegistrations = quotaRegistrationCapability
	policy.QuotaCheckpoint = &quotaCheckpoint{Instance: debit.Grant.Instance, Sequence: strconv.FormatUint(localQuota.sequence, 10)}
	policy.LegacyGroups = []string{debit.Group}
	policy.LegacyQuotaMultipliers = map[string]int64{debit.Group: userQuotaScale}
	var identityExpiry int64
	if identityLifetime > 0 {
		identityExpiry = time.Now().Add(identityLifetime).UnixMilli()
	}
	policy.LegacyOwners = map[string]legacyOwnerPolicy{
		"1": {Allowed: true, ExpiresAt: identityExpiry},
		"2": {Allowed: true, ExpiresAt: identityExpiry},
	}
	policy.Groups[debit.Group] = hostLimitPolicy{
		MultiplierScale: userQuotaScale, ReserveRequired: true,
		UnlimitedOwners: map[string]bool{"2": true},
		Reserves:        map[string]quotaGrant{"1": debit.Grant},
	}
	for host, access := range policy.Hosts {
		access.Groups = append(access.Groups, debit.Group)
		access.QuotaMultipliers = map[string]int64{debit.Group: userQuotaScale}
		access.IdentityExpiresAt = map[string]int64{hostEmail("1", host): identityExpiry, hostEmail("2", host): identityExpiry}
		policy.Hosts[host] = access
	}
	if !publishHostPolicy(policy) {
		t.Fatal("registered global USER policy was not accepted")
	}
	return policy, restore
}

func restartOfflineQuotaPolicy(t *testing.T, saved []byte) {
	t.Helper()
	localQuota = &quotaStore{path: localQuota.path}
	var cached hostPolicyDocument
	if err := json.Unmarshal(saved, &cached); err != nil {
		t.Fatal(err)
	}
	hostPolicies.Lock()
	hostPolicies.document = hostPolicyDocument{}
	hostPolicies.Unlock()
	if !publishHostPolicy(cached) {
		t.Fatal("cached policy and durable ledger could not be restored without the panel")
	}
	file := filepath.Join(filepath.Dir(localQuota.path), "host-policy.json")
	if err := os.WriteFile(file, saved, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateQuotaCheckpointStartupFrom(file); err != nil {
		t.Fatal("startup watermark rejected the saved policy", err)
	}
	if err := validateUserQuotaStartup(); err != nil {
		t.Fatal("startup USER requirement was not preserved", err)
	}
}

func checkOfflineIdentity(t *testing.T, owner string, allowed bool) {
	t.Helper()
	for _, sample := range []struct{ email, inbound, direction string }{
		{owner, "ordinary", "up"},
		{hostEmail(owner, hostA), "shared", "down"},
		{hostEmail(owner, hostB), "shared", "up"},
	} {
		err := waitHostPolicy(policyContext(sample.inbound), sample.email, sample.direction, 1)
		if (err == nil) != allowed {
			t.Fatalf("owner=%s identity=%s virtualNow=%s allowed=%v: %v", owner, sample.email, time.Now(), allowed, err)
		}
	}
}

func addOfflineExpiringGrant(t *testing.T, policy hostPolicyDocument) (hostPolicyDocument, quotaDebit) {
	t.Helper()
	debit := quotaTestDebit("cccccccc-1111-4111-8111-111111111111", 100, 1)
	debit.Owner, debit.Group, debit.Scale = "3", "user:global", userQuotaScale
	debit.Grant.RegistrationRequired = true
	policy.Generation = "mixed-prepare"
	policy.Registrations = registrationPolicy(debit, "prepare").Registrations
	if !publishHostPolicy(policy) {
		t.Fatal("expiring grant preparation rejected")
	}
	policy.Generation = "mixed-active"
	policy.Registrations = nil
	policy.QuotaCheckpoint = &quotaCheckpoint{Instance: debit.Grant.Instance, Sequence: strconv.FormatUint(localQuota.sequence, 10)}
	group := policy.Groups[debit.Group]
	group.Reserves[debit.Owner] = debit.Grant
	policy.Groups[debit.Group] = group
	policy.LegacyOwners[debit.Owner] = legacyOwnerPolicy{Allowed: true}
	for host, access := range policy.Hosts {
		access.AllowedIdentities[hostEmail(debit.Owner, host)] = true
		access.IdentityExpiresAt[hostEmail(debit.Owner, host)] = 0
		policy.Hosts[host] = access
	}
	if !publishHostPolicy(policy) {
		t.Fatal("expiring grant activation rejected")
	}
	if err := localQuota.debit([]quotaDebit{debit}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return policy, debit
}

func compactOfflineQuota(t *testing.T) {
	t.Helper()
	localQuota.Lock()
	defer localQuota.Unlock()
	localQuota.journalBytes = 4 * 1024 * 1024
	if err := localQuota.save(map[string]quotaEntry{}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestCoreAutonomy24Hours(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ceiling = 8 * quotaWriteAhead
		policy, restore := offlineUserQuotaPolicy(t, ceiling, 48*time.Hour, 48*time.Hour)
		defer restore()
		policy, expired := addOfflineExpiringGrant(t, policy)
		saved, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		for hour := 0; hour <= 24; hour++ {
			if hour > 0 {
				time.Sleep(time.Hour)
			}
			if hour == 2 {
				compactOfflineQuota(t)
				if _, exists := localQuota.entries[expired.Grant.ID]; exists {
					t.Fatal("mixed-expiry grant was not compacted")
				}
			}
			if hour == 12 || hour == 24 {
				restartOfflineQuotaPolicy(t, saved)
			}
			checkOfflineIdentity(t, "1", true)
			checkOfflineIdentity(t, "2", true)
			if hour >= 2 {
				checkOfflineIdentity(t, "3", false)
			}
		}
		if time.Since(started) != 24*time.Hour || time.Now().UnixMilli() < policy.ExpiresAt {
			t.Fatal("test did not cross 24 virtual hours and the panel TTL")
		}
		grant := policy.Groups["user:global"].Reserves["1"]
		entry := localQuota.entries[grant.ID]
		remaining := ceiling - entry.consumed
		if remaining <= 0 {
			t.Fatal("sufficient pre-issued reserve did not survive conservative restart accounting")
		}
		if err := waitHostPolicy(policyContext("ordinary"), "1", "up", int(remaining)); err != nil {
			t.Fatal(err)
		}
		checkOfflineIdentity(t, "1", false)
		checkOfflineIdentity(t, "2", true)
		restartOfflineQuotaPolicy(t, saved)
		checkOfflineIdentity(t, "1", false)
		checkOfflineIdentity(t, "2", true)
		t.Log("24 virtual hours; mixed-expiry compaction; registered finite and unlimited global USER; two offline ledger reloads; exhaustion survives another reload")
	})
}

func TestCoreAutonomyExpiry(t *testing.T) {
	for _, identityExpiry := range []bool{false, true} {
		t.Run(strconv.FormatBool(identityExpiry), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				grantLifetime, identityLifetime := 25*time.Hour, time.Duration(0)
				if identityExpiry {
					grantLifetime, identityLifetime = 48*time.Hour, 25*time.Hour
				}
				policy, restore := offlineUserQuotaPolicy(t, 8*quotaWriteAhead, grantLifetime, identityLifetime)
				defer restore()
				saved, _ := json.Marshal(policy)
				time.Sleep(24 * time.Hour)
				restartOfflineQuotaPolicy(t, saved)
				checkOfflineIdentity(t, "1", true)
				checkOfflineIdentity(t, "2", true)
				time.Sleep(time.Hour)
				checkOfflineIdentity(t, "1", false)
				checkOfflineIdentity(t, "2", !identityExpiry)
				restartOfflineQuotaPolicy(t, saved)
				checkOfflineIdentity(t, "1", false)
				checkOfflineIdentity(t, "2", !identityExpiry)
			})
		})
	}
}

func TestCoreAutonomyLoopbackAdmission(t *testing.T) {
	payload := []byte("native global USER admission over loopback")
	policy, restore := offlineUserQuotaPolicy(t, int64(len(payload)), 48*time.Hour, 48*time.Hour)
	defer restore()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	writer := &hostPolicyWriter{Writer: buf.NewWriter(server), ctx: policyContext("shared"), email: hostEmail("1", hostA), direction: "up"}
	if err := writer.WriteMultiBuffer(buf.MergeBytes(nil, payload)); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(client, received); err != nil || !bytes.Equal(payload, received) {
		t.Fatalf("loopback bytes=%q err=%v", received, err)
	}
	if err := writer.WriteMultiBuffer(buf.MergeBytes(nil, []byte("denied"))); err == nil {
		t.Fatal("exhausted finite USER forwarded another buffer")
	}
	saved, _ := json.Marshal(policy)
	restartOfflineQuotaPolicy(t, saved)
	if err := writer.WriteMultiBuffer(buf.MergeBytes(nil, []byte("denied after restart"))); err == nil {
		t.Fatal("restart reopened the exhausted finite USER")
	}
	writer.email = hostEmail("2", hostA)
	if err := writer.WriteMultiBuffer(buf.MergeBytes(nil, payload)); err != nil {
		t.Fatal("unlimited USER was blocked by another USER's exhaustion", err)
	}
	if _, err := io.ReadFull(client, received); err != nil || !bytes.Equal(payload, received) {
		t.Fatalf("denied bytes leaked or unlimited bytes were lost: %q, %v", received, err)
	}
}

func TestCoreAutonomyUnknownLedger(t *testing.T) {
	for _, mutation := range []string{"missing", "corrupt", "partial-frame"} {
		t.Run(mutation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, restore := offlineUserQuotaPolicy(t, 8*quotaWriteAhead, 48*time.Hour, 48*time.Hour)
				defer restore()
				checkOfflineIdentity(t, "1", true)
				time.Sleep(24 * time.Hour)
				file := localQuota.path
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "missing":
					err = os.Remove(file)
				case "corrupt":
					err = os.WriteFile(file, []byte("untrusted ledger"), 0600)
				case "partial-frame":
					err = os.WriteFile(file, data[:len(data)-5], 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				localQuota = &quotaStore{path: file}
				checkOfflineIdentity(t, "1", false)
			})
		})
	}
}
