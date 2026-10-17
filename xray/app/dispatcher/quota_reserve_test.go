//go:build !wasm

package dispatcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
)

func quotaTestStore(t *testing.T) *quotaStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"instance":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","version":"remnacust-quota-reserves-v1","entries":{}}`)
	sum := sha256.Sum256(payload)
	data, _ := json.Marshal(quotaEnvelope{payload, hex.EncodeToString(sum[:])})
	path := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return &quotaStore{path: path}
}
func quotaTestDebit(id string, cap, cost int64) quotaDebit {
	return quotaDebit{Grant: quotaGrant{ID: id, Instance: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Ceiling: strconvInt(cap), ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second).UnixMilli()}, Group: "host:one", Owner: "123", Cost: cost}
}
func strconvInt(n int64) string { return fmt.Sprint(n) }

func TestQuotaReserveCrashNeverReissuesFlushedCredit(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 2*quotaWriteAhead, 1)
	if err := s.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.entries[d.Grant.ID].Reserved <= 1 {
		t.Fatal("no write ahead")
	}
	restarted := &quotaStore{path: s.path}
	d.Cost = quotaWriteAhead
	if err := restarted.debit([]quotaDebit{d}, time.Now()); err == nil {
		t.Fatal("crash replayed unspent block")
	}
	d.Cost = 1
	if err := restarted.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The second crash burns the rest; a replayed identical policy cannot refill it.
	again := &quotaStore{path: s.path}
	if err := again.debit([]quotaDebit{d}, time.Now()); err == nil {
		t.Fatal("policy replay replenished exhausted grant")
	}
}

func TestQuotaReserveAtomicOverlappingScopesAndConcurrentDevices(t *testing.T) {
	s := quotaTestStore(t)
	a := quotaTestDebit("11111111-1111-4111-8111-111111111111", 100, 1)
	b := quotaTestDebit("22222222-2222-4222-8222-222222222222", 50, 1)
	b.Group = "tag:all"
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.debit([]quotaDebit{a, b}, time.Now()) == nil {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 50 || s.entries[a.Grant.ID].consumed != 50 || s.entries[b.Grant.ID].consumed != 50 {
		t.Fatal("non-atomic or excessive quota debit", allowed.Load(), s.entries)
	}
}

func TestQuotaReserveRejectsLossCorruptionBindingExpiryAndWriteFailure(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 2*quotaWriteAhead, 1)
	if err := s.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	other := d
	other.Grant.Instance = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if s.debit([]quotaDebit{other}, time.Now()) == nil {
		t.Fatal("reserve crossed node instance")
	}
	other = d
	other.Owner = "124"
	if s.debit([]quotaDebit{other}, time.Now()) == nil {
		t.Fatal("reserve crossed owner")
	}
	other = d
	other.Group = "tag:wrong"
	if s.debit([]quotaDebit{other}, time.Now()) == nil {
		t.Fatal("reserve crossed scope")
	}
	other = d
	other.Grant.Ceiling = "1"
	if s.debit([]quotaDebit{other}, time.Now()) == nil {
		t.Fatal("older ceiling accepted")
	}
	other = d
	other.Grant.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if s.debit([]quotaDebit{other}, time.Now()) == nil {
		t.Fatal("expired reserve accepted")
	}
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	if (&quotaStore{path: s.path}).debit([]quotaDebit{d}, time.Now()) == nil {
		t.Fatal("missing ledger became fresh quota")
	}
	d.Cost = quotaWriteAhead + 1
	if s.debit([]quotaDebit{d}, time.Now()) == nil {
		t.Fatal("lost ledger was overwritten")
	}
	if s.failure == nil {
		t.Fatal("ambiguous durability did not stop traffic")
	}
	if err := os.WriteFile(s.path, []byte(`{"payload":{},"sha256":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if (&quotaStore{path: s.path}).debit([]quotaDebit{d}, time.Now()) == nil {
		t.Fatal("corruption ignored")
	}
}

func TestQuotaPolicyOutlivesPanelTTLButStopsAtReserveAndIdentityExpiry(t *testing.T) {
	store := quotaTestStore(t)
	before := localQuota
	localQuota = store
	t.Cleanup(func() { localQuota = before })
	policy := v2Policy()
	policy.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
	policy.OfflineReserves = quotaReserveCapability
	email := hostEmail("123", hostA)
	access := policy.Hosts[hostA]
	access.AllowedIdentities[email] = true
	access.IdentityExpiresAt = map[string]int64{email: 0}
	access.QuotaMultipliers = map[string]int64{"host:one": 150}
	access.Groups = []string{"host:one"}
	policy.Hosts[hostA] = access
	debit := quotaTestDebit("11111111-1111-4111-8111-111111111111", 3, 0)
	policy.Groups = map[string]hostLimitPolicy{"host:one": {ReserveRequired: true, Reserves: map[string]quotaGrant{"123": debit.Grant}}}
	testHostPolicy(t, policy)
	if err := waitHostPolicy(policyContext("shared"), email, "up", 1); err != nil {
		t.Fatal(err)
	} // first fractional charge is provisionally rounded to 2 bytes
	if err := waitHostPolicy(policyContext("shared"), email, "down", 1); err != nil {
		t.Fatal("two raw bytes at 1.50 must fit exactly in three billed bytes", err)
	}
	if waitHostPolicy(policyContext("shared"), email, "down", 1) == nil {
		t.Fatal("uplink/downlink did not share weighted reserve")
	}
	hostPolicies.Lock()
	access.IdentityExpiresAt[email] = time.Now().Add(-time.Second).UnixMilli()
	hostPolicies.document.Hosts[hostA] = access
	hostPolicies.Unlock()
	if waitHostPolicy(policyContext("shared"), email, "up", 0) == nil {
		t.Fatal("expired subscription allowed offline")
	}
}

func TestQuotaJournalAppendsOnlyChangedGrantsAndRejectsTruncation(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 10*quotaWriteAhead, 1)
	if err := s.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	d.Cost = quotaWriteAhead + 1
	if err := s.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	frames := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	if len(frames) != 2 {
		t.Fatalf("expected checkpoint plus incremental record, got %d", len(frames))
	}
	restarted := &quotaStore{path: s.path}
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if restarted.entries[d.Grant.ID].consumed != s.entries[d.Grant.ID].Reserved {
		t.Fatal("restart lost flushed high water")
	}
	if err := os.WriteFile(s.path, data[:len(data)-5], 0600); err != nil {
		t.Fatal(err)
	}
	if (&quotaStore{path: s.path}).load() == nil {
		t.Fatal("partial quota journal ignored")
	}
	if err := os.WriteFile(s.path, append(data, frames[1]...), 0600); err != nil {
		t.Fatal(err)
	}
	if (&quotaStore{path: s.path}).load() == nil {
		t.Fatal("duplicate or incomplete sequence accepted")
	}
}

func TestQuotaJournalCompactionCannotResurrectExpiredGrantAfterClockRollback(t *testing.T) {
	s := quotaTestStore(t)
	now := time.Now()
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 10, 1)
	if err := s.debit([]quotaDebit{d}, now); err != nil {
		t.Fatal(err)
	}
	s.journalBytes = 4 * 1024 * 1024
	next := quotaTestDebit("22222222-2222-4222-8222-222222222222", 10, 1)
	next.Grant.ExpiresAt = d.Grant.ExpiresAt + 3600000
	if err := s.debit([]quotaDebit{next}, time.UnixMilli(d.Grant.ExpiresAt+1)); err != nil {
		t.Fatal(err)
	}
	if _, exists := s.entries[d.Grant.ID]; exists {
		t.Fatal("expired grant not compacted")
	}
	restarted := &quotaStore{path: s.path}
	if restarted.debit([]quotaDebit{d}, now) == nil {
		t.Fatal("clock rollback resurrected compacted credit")
	}
}

func TestCachedProtectedInboundCannotFallBackToLegacyKeysWhenPolicyIsLost(t *testing.T) {
	instance := &core.Instance{}
	instance.RecordConfiguration(&core.Config{Inbound: []*core.InboundHandlerConfig{{Tag: "shared", ProxySettings: serial.ToTypedMessage(&protocol.User{Email: hostEmail("123", hostA)})}}})
	testHostPolicy(t, hostPolicyDocument{})
	ctx := context.WithValue(policyContext("shared"), core.XrayKey(1), instance)
	if waitHostPolicy(ctx, "123", "up", 1) == nil {
		t.Fatal("lost policy reopened a protected listener for legacy keys")
	}
	unrelated := context.WithValue(policyContext("ordinary"), core.XrayKey(1), instance)
	if err := waitHostPolicy(unrelated, "123", "up", 1); err != nil {
		t.Fatal("unrelated listener stopped with the protected one", err)
	}
}

func TestQuotaFenceClosesAndSettlesAReserveBeforeItCanBeRedistributed(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 10*quotaWriteAhead, 123)
	if err := s.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.entries[d.Grant.ID].Reserved <= 123 {
		t.Fatal("test did not prewrite a block")
	}
	if err := s.reconcile(hostPolicyDocument{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	entry := s.entries[d.Grant.ID]
	if !entry.Closed || entry.Reserved != 123 {
		t.Fatal("clean fencing did not persist the final charge", entry)
	}
	if s.debit([]quotaDebit{d}, time.Now()) == nil {
		t.Fatal("closed grant was reopened")
	}
	restarted := &quotaStore{path: s.path}
	if restarted.debit([]quotaDebit{d}, time.Now()) == nil {
		t.Fatal("restart reopened a closed grant")
	}
	if restarted.entries[d.Grant.ID].Reserved != 123 {
		t.Fatal("settled charge was lost")
	}
	policy := hostPolicyDocument{Groups: map[string]hostLimitPolicy{d.Group: {ReserveRequired: true, Reserves: map[string]quotaGrant{d.Owner: d.Grant}}}}
	if restarted.reconcile(policy, time.Now()) == nil {
		t.Fatal("a newer policy revision resurrected a permanently closed reserve")
	}
}

func TestQuotaAcknowledgementRegistersUnusedGrantsBeforeTheyCanBeSealed(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("cccccccc-cccc-4ccc-8ccc-cccccccccccc", 10*quotaWriteAhead, 1)
	policy := hostPolicyDocument{Groups: map[string]hostLimitPolicy{d.Group: {ReserveRequired: true, Reserves: map[string]quotaGrant{d.Owner: d.Grant}}}}
	if err := s.reconcile(policy, time.Now()); err != nil {
		t.Fatal(err)
	}
	crashed := &quotaStore{path: s.path}
	if err := crashed.load(); err != nil {
		t.Fatal(err)
	}
	entry, exists := crashed.entries[d.Grant.ID]
	if !exists || entry.Ceiling != 10*quotaWriteAhead || entry.Reserved != 0 {
		t.Fatal("unused grant was not persisted before acknowledgement", entry)
	}
	if err := crashed.reconcile(hostPolicyDocument{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	closed := &quotaStore{path: s.path}
	if err := closed.load(); err != nil {
		t.Fatal(err)
	}
	entry = closed.entries[d.Grant.ID]
	if !entry.Closed || entry.Reserved != 0 {
		t.Fatal("unused grant cannot prove its final zero charge", entry)
	}
	if err := closed.reconcile(policy, time.Now()); err == nil {
		t.Fatal("zero-usage closure reopened the grant")
	}
}

func TestQuotaFractionalBillingAccumulatesAcrossTinyPacketsAndDevices(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("dddddddd-dddd-4ddd-8ddd-dddddddddddd", 1, 0)
	d.Fraction = 1
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.debit([]quotaDebit{d}, time.Now()) == nil {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 100 || s.entries[d.Grant.ID].consumed != 1 || s.entries[d.Grant.ID].fraction != 0 {
		t.Fatal("per-packet rounding distorted the multiplier", allowed.Load(), s.entries[d.Grant.ID])
	}
	// A crash may conservatively burn prepaid bytes, but cannot recreate the
	// fractional allowance already included in that byte.
	restarted := &quotaStore{path: s.path}
	if err := restarted.debit([]quotaDebit{d}, time.Now()); err == nil {
		t.Fatal("crash recreated fractional credit")
	}
}

func TestQuotaFractionalBillingIsIndependentOfBufferSegmentation(t *testing.T) {
	for _, factor := range []int64{1, 50, 99, 100, 101, 150, 9999, 10000} {
		t.Run(strconvInt(factor), func(t *testing.T) {
			a, b := quotaTestStore(t), quotaTestStore(t)
			d := quotaTestDebit("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", 100000, 0)
			d.Cost, d.Fraction = 137*factor/100, 137*factor%100
			if err := a.debit([]quotaDebit{d}, time.Now()); err != nil {
				t.Fatal(err)
			}
			d.Cost, d.Fraction = factor/100, factor%100
			for i := 0; i < 137; i++ {
				if err := b.debit([]quotaDebit{d}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			left, right := a.entries[d.Grant.ID], b.entries[d.Grant.ID]
			if left.consumed != right.consumed || left.fraction != right.fraction {
				t.Fatal("segmentation changed billing", left, right)
			}
		})
	}
}
