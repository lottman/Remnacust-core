//go:build !wasm

package dispatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/core"
)

const userQuotaCapability = "remnacust-user-quota-reserves-v1"
const userQuotaScale int64 = 1000000000
const maxUserQuotaFactor int64 = 100 * userQuotaScale

type legacyOwnerPolicy struct {
	Allowed   bool  `json:"allowed"`
	ExpiresAt int64 `json:"expiresAt"`
}

var userQuotaObserved atomic.Bool

func validQuotaScale(scale int64) bool { return scale == 0 || scale == 100 || scale == userQuotaScale }
func quotaScale(scale int64) int64 {
	if scale == 0 {
		return 100
	}
	return scale
}
func storedQuotaScale(scale int64) int64 {
	if scale == 100 {
		return 0
	}
	return scale
}

func quotaChargeFactor(policy hostPolicyDocument, host, key string, group hostLimitPolicy, size int) (int64, int64, error) {
	scale := quotaScale(group.MultiplierScale)
	var factor int64
	var found bool
	if host == "" {
		factor, found = policy.LegacyQuotaMultipliers[key]
	} else {
		factor, found = policy.Hosts[host].QuotaMultipliers[key]
	}
	minimum, maximum := int64(1), int64(10000)
	if scale == userQuotaScale {
		minimum, maximum = 0, maxUserQuotaFactor
	}
	if !validQuotaScale(group.MultiplierScale) || !found || factor < minimum || factor > maximum || size < 0 || (factor > 0 && int64(size) > (1<<63-1)/factor) {
		return 0, 0, errors.New("invalid quota billing factor")
	}
	return factor, scale, nil
}

func selectedLegacyLimits(owner string, policy hostPolicyDocument) (map[string]hostLimitPolicy, error) {
	denied := errors.New("legacy identity access or quota denied traffic")
	result := make(map[string]hostLimitPolicy)
	autonomous := len(policy.LegacyGroups) > 0
	if autonomous {
		identity, known := policy.LegacyOwners[owner]
		if policy.UserQuota != userQuotaCapability || policy.OfflineReserves != quotaReserveCapability || !known || !identity.Allowed || identity.ExpiresAt < 0 || (identity.ExpiresAt != 0 && time.Now().UnixMilli() >= identity.ExpiresAt) {
			return nil, denied
		}
		for _, key := range policy.LegacyGroups {
			group, exists := policy.Groups[key]
			if !exists {
				return nil, denied
			}
			result[key] = group
		}
	}
	if policy.Group != "" {
		if owner == "" || (!autonomous && time.Now().UnixMilli() >= policy.ExpiresAt) {
			return nil, denied
		}
		if _, collision := result[policy.Group]; collision {
			return nil, denied
		}
		result[policy.Group] = hostLimitPolicy{BytesPerSecond: policy.BytesPerSecond, BlockAll: policy.BlockAll, BlockedOwners: policy.BlockedOwners}
	}
	return result, nil
}

func validateUserQuotaPolicy(policy hostPolicyDocument) error {
	used := policy.UserQuota != "" || policy.LegacyGroups != nil || policy.LegacyOwners != nil || policy.LegacyQuotaMultipliers != nil
	for _, group := range policy.Groups {
		if !validQuotaScale(group.MultiplierScale) {
			return errors.New("invalid quota precision")
		}
		used = used || group.MultiplierScale == userQuotaScale
	}
	for _, access := range policy.Hosts {
		used = used || access.UserQuotaExempt
	}
	if !used {
		return nil
	}
	if policy.UserQuota != userQuotaCapability || policy.OfflineReserves != quotaReserveCapability || len(policy.LegacyGroups) == 0 || len(policy.LegacyGroups) > 200 || policy.LegacyOwners == nil || policy.LegacyQuotaMultipliers == nil || len(policy.LegacyOwners) > 200000 {
		return errors.New("invalid autonomous user quota policy")
	}
	seen := make(map[string]bool)
	for _, key := range policy.LegacyGroups {
		group, exists := policy.Groups[key]
		factor, priced := policy.LegacyQuotaMultipliers[key]
		if seen[key] || !exists || !group.ReserveRequired || group.MultiplierScale != userQuotaScale || !priced || factor < 0 || factor > maxUserQuotaFactor {
			return errors.New("invalid legacy user quota binding")
		}
		seen[key] = true
	}
	for owner, identity := range policy.LegacyOwners {
		id, err := strconv.ParseInt(owner, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != owner || identity.ExpiresAt < 0 || identity.ExpiresAt > 253402300799999 {
			return errors.New("invalid legacy user entitlement")
		}
	}
	for _, access := range policy.Hosts {
		bound := make(map[string]bool, len(access.Groups))
		for _, key := range access.Groups {
			bound[key] = true
		}
		for _, key := range policy.LegacyGroups {
			if bound[key] == access.UserQuotaExempt {
				return errors.New("host is missing its global quota or has an ambiguous recovery exemption")
			}
		}
		for _, key := range access.Groups {
			if group, exists := policy.Groups[key]; exists && group.MultiplierScale == userQuotaScale {
				factor, priced := access.QuotaMultipliers[key]
				if !group.ReserveRequired || !priced || factor < 0 || factor > maxUserQuotaFactor {
					return errors.New("invalid host user quota binding")
				}
			}
		}
	}
	return nil
}

func userQuotaRequirementPath() string {
	return filepath.Join(filepath.Dir(localQuota.path), "user-quota-required")
}

func userQuotaRequired() (bool, error) {
	path := userQuotaRequirementPath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 128 {
		return false, errors.New("invalid user quota requirement marker")
	}
	data, err := quotaFile(path)
	if err != nil {
		return false, err
	}
	if string(data) != userQuotaCapability+"\n" {
		return false, errors.New("unknown user quota requirement marker")
	}
	return true, nil
}

func ensureUserQuotaRequired() error {
	required, err := userQuotaRequired()
	if err != nil || required {
		return err
	}
	localQuota.Lock()
	defer localQuota.Unlock()
	if err := localQuota.load(); err != nil {
		return err
	}
	path := userQuotaRequirementPath()
	file, err := os.CreateTemp(filepath.Dir(path), ".user-quota-required-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString(userQuotaCapability + "\n")
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = directory.Sync()
		closeErr := directory.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	return nil
}

func UserQuotaPolicyReady() bool {
	hostPolicies.Lock()
	defer hostPolicies.Unlock()
	return hostPolicies.document.UserQuota == userQuotaCapability && hostPolicies.document.OfflineReserves == quotaReserveCapability
}

// A receipt marker is not a configuration. Its loss or a partial cached policy
// must never turn authenticated legacy identities into unmetered traffic.
func validateUserQuotaStartup() error {
	required, err := userQuotaRequired()
	if err != nil {
		return err
	}
	if (required || userQuotaObserved.Load()) && !UserQuotaPolicyReady() {
		return errors.New("saved user quota policy could not be recovered")
	}
	return nil
}

func prepareHostPolicy(ctx context.Context) error {
	if err := startHostPolicy(); err != nil {
		return err
	}
	if ctx != nil {
		if instance := core.FromContext(ctx); instance != nil && instance.RequiresUserQuota() && !UserQuotaPolicyReady() {
			return errors.New("cached credentials require the saved user quota policy")
		}
	}
	return nil
}
