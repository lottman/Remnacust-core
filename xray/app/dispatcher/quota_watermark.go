//go:build !wasm

package dispatcher

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
)

const quotaWatermarkCapability = "remnacust-quota-watermarks-v1"

type quotaCheckpoint struct {
	Instance string `json:"instance"`
	Sequence string `json:"sequence"`
}

func quotaJournalVersion(version string) int {
	switch version {
	case quotaReserveCapability:
		return 1
	case quotaRegistrationCapability:
		return 2
	case quotaWatermarkCapability:
		return 3
	default:
		return 0
	}
}

func (s *quotaStore) validateCheckpoint(policy hostPolicyDocument) (uint64, error) {
	if policy.QuotaCheckpoint == nil {
		if s.watermarks {
			return 0, errors.New("quota journal checkpoint cannot be removed")
		}
		return 0, nil
	}
	checkpoint := policy.QuotaCheckpoint
	sequence, err := strconv.ParseUint(checkpoint.Sequence, 10, 64)
	if err != nil || strconv.FormatUint(sequence, 10) != checkpoint.Sequence || checkpoint.Instance != s.instance || sequence > s.sequence || sequence < s.checkpoint || policy.QuotaRegistrations != quotaRegistrationCapability {
		return 0, errors.New("quota journal is behind its acknowledged checkpoint; recovery required")
	}
	return sequence, nil
}

func validateQuotaCheckpointStartup() error {
	var saved hostPolicyDocument
	data, policyErr := os.ReadFile(hostPolicyPath)
	if policyErr == nil && json.Unmarshal(data, &saved) != nil {
		return errors.New("saved quota policy is invalid")
	}
	if policyErr != nil && !os.IsNotExist(policyErr) {
		return policyErr
	}
	hostPolicies.Lock()
	applied := hostPolicies.document
	hostPolicies.Unlock()
	localQuota.Lock()
	defer localQuota.Unlock()
	if err := localQuota.load(); err != nil {
		if os.IsNotExist(err) && saved.OfflineReserves == "" {
			return nil
		}
		return err
	}
	if saved.QuotaCheckpoint != nil {
		if _, err := localQuota.validateCheckpoint(saved); err != nil {
			return err
		}
		if applied.Generation != saved.Generation || applied.QuotaCheckpoint == nil {
			return errors.New("saved quota checkpoint was not applied before startup")
		}
	}
	if _, err := localQuota.validateCheckpoint(applied); err != nil {
		return err
	}
	return nil
}
