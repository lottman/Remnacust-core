//go:build !wasm

package dispatcher

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

func TestAuditMixedExpiryStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		policy, restore := offlineUserQuotaPolicy(t, 8*quotaWriteAhead, 48*time.Hour, 48*time.Hour)
		defer restore()
		policy, expired := addOfflineExpiringGrant(t, policy)
		saved, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(24 * time.Hour)
		compactOfflineQuota(t)
		if _, exists := localQuota.entries[expired.Grant.ID]; exists || localQuota.retiredBefore < expired.Grant.ExpiresAt {
			t.Fatal("expired grant was not durably retired")
		}
		restartOfflineQuotaPolicy(t, saved)
		checkOfflineIdentity(t, "1", true)
		checkOfflineIdentity(t, "2", true)
		checkOfflineIdentity(t, "3", false)
	})
}

func TestQuotaRetiredPolicyReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		policy, restore := offlineUserQuotaPolicy(t, 8*quotaWriteAhead, 48*time.Hour, 48*time.Hour)
		defer restore()
		policy, expired := addOfflineExpiringGrant(t, policy)
		issuedAt := time.Now()
		saved, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Hour)
		compactOfflineQuota(t)
		retired, checkpoint := localQuota.retiredBefore, localQuota.checkpoint
		if retired < expired.Grant.ExpiresAt || checkpoint == 0 {
			t.Fatal("fixture did not persist retirement and a native watermark")
		}
		restartOfflineQuotaPolicy(t, saved)
		for _, required := range []bool{true, false} {
			var replay hostPolicyDocument
			if err := json.Unmarshal(saved, &replay); err != nil {
				t.Fatal(err)
			}
			grant := expired.Grant
			grant.Ceiling = "1000000"
			grant.RegistrationRequired = required
			group := replay.Groups[expired.Group]
			group.Reserves[expired.Owner] = grant
			replay.Groups[expired.Group] = group
			recovered := &quotaStore{path: localQuota.path}
			if err := recovered.reconcile(replay, time.Now()); err != nil {
				t.Fatal("an inert retired reference blocked the valid grants", err)
			}
			if _, exists := recovered.entries[grant.ID]; exists {
				t.Fatal("retired reference recreated a journal entry")
			}
			if recovered.retiredBefore < retired || recovered.checkpoint < checkpoint {
				t.Fatal("policy replay lowered retirement or the watermark")
			}
			attempt := expired
			attempt.Grant = grant
			for _, now := range []time.Time{time.Now(), issuedAt} {
				if err := recovered.debit([]quotaDebit{attempt}, now); err == nil {
					t.Fatal("retired credit was restored, including after wall-clock rollback")
				}
			}
		}
		checkOfflineIdentity(t, "1", true)
		checkOfflineIdentity(t, "2", true)
		checkOfflineIdentity(t, "3", false)

		for _, mutation := range []string{"retime", "instance", "ceiling", "duplicate", "checkpoint", "protocol"} {
			var invalid hostPolicyDocument
			if err := json.Unmarshal(saved, &invalid); err != nil {
				t.Fatal(err)
			}
			group := invalid.Groups[expired.Group]
			grant := group.Reserves[expired.Owner]
			switch mutation {
			case "retime":
				grant.ExpiresAt = issuedAt.Add(48 * time.Hour).UnixMilli()
			case "instance":
				grant.Instance = "dddddddd-1111-4111-8111-111111111111"
			case "ceiling":
				grant.Ceiling = "invalid"
			case "duplicate":
				invalid.Groups["duplicate"] = hostLimitPolicy{ReserveRequired: true, MultiplierScale: userQuotaScale, Reserves: map[string]quotaGrant{expired.Owner: grant}}
			case "checkpoint":
				invalid.QuotaCheckpoint.Sequence = "0"
			case "protocol":
				invalid.QuotaRegistrations = ""
			}
			group.Reserves[expired.Owner] = grant
			invalid.Groups[expired.Group] = group
			before, err := os.ReadFile(localQuota.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := (&quotaStore{path: localQuota.path}).reconcile(invalid, time.Now()); err == nil {
				t.Fatalf("invalid retired replay accepted: %s", mutation)
			}
			after, err := os.ReadFile(localQuota.path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected %s replay mutated the ledger: %v", mutation, err)
			}
		}
	})
}
