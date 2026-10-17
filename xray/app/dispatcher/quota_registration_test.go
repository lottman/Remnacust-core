//go:build !wasm

package dispatcher

import (
	"os"
	"testing"
	"time"
)

func registrationPolicy(d quotaDebit, action string) hostPolicyDocument {
	return hostPolicyDocument{OfflineReserves: quotaReserveCapability, QuotaRegistrations: quotaRegistrationCapability,
		Groups: map[string]hostLimitPolicy{}, Registrations: []quotaRegistration{{Grant: d.Grant, Group: d.Group, Owner: d.Owner, Scale: d.Scale, Action: action, AllowMissing: action == "cancel"}}}
}

func activationPolicy(d quotaDebit) hostPolicyDocument {
	p := registrationPolicy(d, "prepare")
	p.Registrations = nil
	p.Groups[d.Group] = hostLimitPolicy{ReserveRequired: true, MultiplierScale: d.Scale, Reserves: map[string]quotaGrant{d.Owner: d.Grant}}
	return p
}

func TestRegisteredQuotaRequiresSeparateDurablePreparation(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 100, 4)
	d.Grant.RegistrationRequired = true
	now := time.Now()
	if s.reconcile(activationPolicy(d), now) == nil || s.debit([]quotaDebit{d}, now) == nil {
		t.Fatal("unknown grant was activated")
	}
	if err := s.reconcile(registrationPolicy(d, "prepare"), now); err != nil {
		t.Fatal(err)
	}
	if s.debit([]quotaDebit{d}, now) == nil {
		t.Fatal("preparation was spendable")
	}
	// Missing stage ACK and another batch do not silently close staged credit.
	p := activationPolicy(d)
	p.Groups = nil
	if err := s.reconcile(p, now); err != nil {
		t.Fatal(err)
	}
	restarted := &quotaStore{path: s.path}
	if err := restarted.reconcile(registrationPolicy(d, "prepare"), now); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcile(activationPolicy(d), now); err != nil {
		t.Fatal(err)
	}
	if err := restarted.debit([]quotaDebit{d}, now); err != nil {
		t.Fatal(err)
	}
	if restarted.entries[d.Grant.ID].consumed != 4 {
		t.Fatal("wrong charge")
	}
	if restarted.reconcile(registrationPolicy(d, "prepare"), now) == nil {
		t.Fatal("used credit was restaged")
	}
	if err := restarted.reconcile(registrationPolicy(d, "cancel"), now); err != nil {
		t.Fatal(err)
	}
	if e := restarted.entries[d.Grant.ID]; !e.Closed || e.Reserved != 4 {
		t.Fatal("cancel lost real usage", e)
	}
	if restarted.reconcile(activationPolicy(d), now) == nil {
		t.Fatal("cancelled credit reopened")
	}
	if err := (&quotaStore{path: s.path}).load(); err != nil {
		t.Fatal(err)
	}
}

func TestRegisteredQuotaUnknownCancelIsDurableAndLegacyNeverZeroed(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 100, 0)
	d.Grant.RegistrationRequired = true
	p := registrationPolicy(d, "cancel")
	p.Registrations[0].AllowMissing = false
	if s.reconcile(p, time.Now()) == nil {
		t.Fatal("missing previously activated credit was zeroed")
	}
	p.Registrations[0].AllowMissing = true
	if err := s.reconcile(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := &quotaStore{path: s.path}
	if err := r.load(); err != nil {
		t.Fatal(err)
	}
	if e := r.entries[d.Grant.ID]; !e.Prepared || !e.Closed || e.Reserved != 0 {
		t.Fatal(e)
	}
	if err := r.reconcile(p, time.Now()); err != nil {
		t.Fatal("lost ACK cancel replay", err)
	}
	if r.reconcile(registrationPolicy(d, "prepare"), time.Now()) == nil {
		t.Fatal("closed preparation replay")
	}
	d.Grant.ID = "22222222-2222-4222-8222-222222222222"
	d.Grant.RegistrationRequired = false
	if err := r.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	d.Grant.RegistrationRequired = true
	if r.reconcile(registrationPolicy(d, "cancel"), time.Now()) == nil {
		t.Fatal("legacy entry treated as never delivered")
	}
}

func TestRegisteredQuotaRejectsWholeInvalidBatchBeforeAnyWrite(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 100, 0)
	d.Grant.RegistrationRequired = true
	for _, invalid := range []string{"duplicate", "owner", "instance", "action", "active"} {
		t.Run(invalid, func(t *testing.T) {
			p := registrationPolicy(d, "prepare")
			bad := p.Registrations[0]
			bad.Grant.ID = "22222222-2222-4222-8222-222222222222"
			switch invalid {
			case "duplicate":
				bad.Grant.ID = d.Grant.ID
			case "owner":
				bad.Owner = "0123"
			case "instance":
				bad.Grant.Instance = bad.Grant.ID
			case "action":
				bad.Action = "reset"
			case "active":
				p.Groups = activationPolicy(d).Groups
			}
			p.Registrations = append(p.Registrations, bad)
			before, _ := os.ReadFile(s.path)
			if s.reconcile(p, time.Now()) == nil {
				t.Fatal("invalid batch accepted")
			}
			after, _ := os.ReadFile(s.path)
			if string(before) != string(after) {
				t.Fatal("partial journal mutation")
			}
		})
	}
}

func TestRegisteredQuotaCrashKeepsPrewrittenUsageAndStickyVersion(t *testing.T) {
	s := quotaTestStore(t)
	d := quotaTestDebit("11111111-1111-4111-8111-111111111111", 2*quotaWriteAhead, 1)
	d.Grant.RegistrationRequired = true
	for _, p := range []hostPolicyDocument{registrationPolicy(d, "prepare"), activationPolicy(d)} {
		if err := s.reconcile(p, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.debit([]quotaDebit{d}, time.Now()); err != nil {
		t.Fatal(err)
	}
	prepaid := s.entries[d.Grant.ID].Reserved
	r := &quotaStore{path: s.path}
	if err := r.reconcile(registrationPolicy(d, "cancel"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if e := r.entries[d.Grant.ID]; !e.Closed || e.Reserved != prepaid || prepaid <= 1 {
		t.Fatal("crash refunded prewritten credit", e)
	}
	if r.reconcile(hostPolicyDocument{}, time.Now()) == nil {
		t.Fatal("protocol downgrade accepted")
	}
	data, err := quotaRecord(map[string]quotaEntry{}, r.retiredBefore, r.sequence+1, r.instance)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(data)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if (&quotaStore{path: s.path}).load() == nil {
		t.Fatal("journal version downgrade accepted")
	}
}
