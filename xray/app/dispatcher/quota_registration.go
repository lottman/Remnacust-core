//go:build !wasm

package dispatcher

import (
	"errors"
	"strconv"
	"time"
)

// This is also the journal version after enrollment. Older cores reject that
// journal rather than silently interpreting prepared credit as active credit.
const quotaRegistrationCapability = "remnacust-quota-registrations-v1"

type quotaRegistration struct {
	Grant        quotaGrant `json:"grant"`
	Group        string     `json:"group"`
	Owner        string     `json:"owner"`
	Scale        int64      `json:"scale,omitempty"`
	Action       string     `json:"action"`
	AllowMissing bool       `json:"allowMissing,omitempty"`
}

// Plans the entire batch before any journal mutation. A cancel for an unknown
// identifier is valid only for the new protocol: the panel cannot activate
// that grant until its durable preparation has been acknowledged in SQL.
func (s *quotaStore) planRegistrations(policy hostPolicyDocument, now time.Time) (map[string]quotaEntry, error) {
	updates := make(map[string]quotaEntry)
	if policy.QuotaRegistrations == "" {
		if s.registeredGrants || len(policy.Registrations) != 0 {
			return nil, errors.New("registered quota protocol cannot be removed")
		}
		return updates, nil
	}
	if policy.QuotaRegistrations != quotaRegistrationCapability || policy.OfflineReserves != quotaReserveCapability || len(policy.Registrations) > 500 {
		return nil, errors.New("invalid registered quota protocol")
	}
	for _, registration := range policy.Registrations {
		grant := registration.Grant
		owner, ownerErr := strconv.ParseInt(registration.Owner, 10, 64)
		ceiling, ceilingErr := strconv.ParseInt(grant.Ceiling, 10, 64)
		if !grant.RegistrationRequired || !validQuotaID(grant.ID) || grant.Instance != s.instance || ownerErr != nil || owner <= 0 || strconv.FormatInt(owner, 10) != registration.Owner || registration.Group == "" || len(registration.Group) > 200 || !validQuotaScale(registration.Scale) || ceilingErr != nil || ceiling < 0 || strconv.FormatInt(ceiling, 10) != grant.Ceiling || grant.ExpiresAt <= s.retiredBefore || grant.ExpiresAt <= now.UnixMilli() || (registration.Action != "prepare" && registration.Action != "cancel") {
			return nil, errors.New("invalid quota registration binding")
		}
		if _, duplicate := updates[grant.ID]; duplicate {
			return nil, errors.New("duplicate quota registration")
		}
		entry, exists := s.entries[grant.ID]
		if registration.Action == "prepare" && registration.AllowMissing {
			return nil, errors.New("prepare cannot assert cancellation proof")
		}
		if !exists && registration.Action == "cancel" && !registration.AllowMissing {
			return nil, errors.New("previously activated quota is missing; recovery required")
		}
		if exists && (!entry.Prepared || entry.Group != registration.Group || entry.Owner != registration.Owner || entry.ExpiresAt != grant.ExpiresAt || quotaScale(entry.Scale) != quotaScale(registration.Scale) || ceiling < entry.Ceiling) {
			return nil, errors.New("quota registration conflicts with durable binding")
		}
		if !exists {
			entry = quotaEntry{Group: registration.Group, Owner: registration.Owner, Ceiling: ceiling, ExpiresAt: grant.ExpiresAt, Scale: storedQuotaScale(quotaScale(registration.Scale)), Prepared: true}
		}
		if registration.Action == "prepare" {
			if entry.Closed || entry.Activated {
				return nil, errors.New("used or closed quota cannot be prepared again")
			}
		} else {
			// Closing known used credit retains its actual in-process charge;
			// after restart consumed already includes the prewritten block.
			entry.Closed = true
			entry.Reserved = entry.consumed
		}
		entry.Ceiling = ceiling
		updates[grant.ID] = entry
	}
	return updates, nil
}
