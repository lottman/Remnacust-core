//go:build !wasm

package dispatcher

import (
	"os"
	"testing"
	"time"
)

func TestQuotaWatermarkRejectsRestoredJournalAndBackwardPolicyBeforeWrites(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 100, 1)
	d.Grant.RegistrationRequired = true
	old, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	p := registrationPolicy(d, "prepare")
	p.QuotaCheckpoint = &quotaCheckpoint{Instance: d.Grant.Instance, Sequence: "0"}
	if err := s.reconcile(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	p = activationPolicy(d)
	p.QuotaCheckpoint = &quotaCheckpoint{Instance: d.Grant.Instance, Sequence: strconvInt(int64(s.sequence))}
	if err := s.reconcile(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !s.watermarks || s.checkpoint == 0 {
		t.Fatal("checkpoint not enrolled")
	}
	for _, value := range []string{"0", "01", "-1", "999999999999999999999"} {
		bad := p
		bad.QuotaCheckpoint = &quotaCheckpoint{Instance: d.Grant.Instance, Sequence: value}
		before, _ := os.ReadFile(s.path)
		if s.reconcile(bad, time.Now()) == nil {
			t.Fatal("invalid/backward checkpoint accepted", value)
		}
		after, _ := os.ReadFile(s.path)
		if string(before) != string(after) {
			t.Fatal("invalid checkpoint mutated journal")
		}
	}
	restarted := &quotaStore{path: s.path}
	if err := restarted.reconcile(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if (&quotaStore{path: s.path}).reconcile(p, time.Now()) == nil {
		t.Fatal("restored old journal accepted against saved policy")
	}
}

func TestQuotaWatermarkJournalVersionAndFloorCannotRewind(t *testing.T) {
	s := quotaTestStore(t)
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	strict, err := quotaCheckpointRecord(map[string]quotaEntry{}, 0, 1, s.instance, true, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, strict, 0600); err != nil {
		t.Fatal(err)
	}
	loaded := &quotaStore{path: s.path}
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	if !loaded.watermarks || loaded.checkpoint != 1 {
		t.Fatal("watermark lost")
	}
	for _, version := range []int{2, 3} {
		frame, err := quotaCheckpointRecord(map[string]quotaEntry{}, 0, 2, s.instance, true, version == 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.path, append(append([]byte{}, strict...), frame...), 0600); err != nil {
			t.Fatal(err)
		}
		if (&quotaStore{path: s.path}).load() == nil {
			t.Fatal("watermark or version rolled back", version)
		}
	}
}
