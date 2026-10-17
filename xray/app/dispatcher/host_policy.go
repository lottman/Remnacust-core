//go:build !wasm

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/quotausage"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/transport"
	"golang.org/x/net/idna"
	"golang.org/x/time/rate"
)

const hostPolicyCapability = "xera-host-policy-v2"
const destinationRulesCapability = "xera-destination-rules-v1"
const legacyHostPolicyCapability = "xera-host-policy-v1"
const hostPolicyPath = "/var/lib/remnanode/host-policy.json"
const policyBurst = 65536 // supports a complete UDP datagram; shared across all connections

type hostPolicyDocument struct {
	Version                string                       `json:"version"`
	Generation             string                       `json:"generation"`
	OfflineReserves        string                       `json:"offlineReserves,omitempty"`
	QuotaRegistrations     string                       `json:"quotaRegistrations,omitempty"`
	QuotaCheckpoint        *quotaCheckpoint             `json:"quotaCheckpoint,omitempty"`
	Registrations          []quotaRegistration          `json:"registrations,omitempty"`
	UserQuota              string                       `json:"userQuota,omitempty"`
	LegacyGroups           []string                     `json:"legacyGroups,omitempty"`
	LegacyOwners           map[string]legacyOwnerPolicy `json:"legacyOwners,omitempty"`
	LegacyQuotaMultipliers map[string]int64             `json:"legacyQuotaMultipliers,omitempty"`
	ExpiresAt              int64                        `json:"expiresAt"`
	ProtectedInbounds      []string                     `json:"protectedInbounds"`
	Hosts                  map[string]hostAccessPolicy  `json:"hosts"`
	Groups                 map[string]hostLimitPolicy   `json:"groups"`
	Group                  string                       `json:"group"`
	BytesPerSecond         int64                        `json:"bytesPerSecond"`
	BlockedOwners          map[string]bool              `json:"blockedOwners"`
	BlockAll               bool                         `json:"blockAll"`
}

type hostAccessPolicy struct {
	UserQuotaExempt   bool             `json:"userQuotaExempt,omitempty"`
	InboundTag        string           `json:"inboundTag"`
	AllowedIdentities map[string]bool  `json:"allowedIdentities"`
	Groups            []string         `json:"groups"`
	DomainMode        string           `json:"domainMode"`
	Domains           []string         `json:"domains"`
	QuotaMultipliers  map[string]int64 `json:"quotaMultipliers,omitempty"`
	IdentityExpiresAt map[string]int64 `json:"identityExpiresAt,omitempty"`
	destinations      *hostDestinations
}
type hostLimitPolicy struct {
	UnreservedAccounts  map[string]string     `json:"unreservedAccounts,omitempty"`
	MultiplierScale     int64                 `json:"multiplierScale,omitempty"`
	FenceAll            bool                  `json:"fenceAll,omitempty"`
	FencedOwners        map[string]bool       `json:"fencedOwners,omitempty"`
	ReserveRequired     bool                  `json:"reserveRequired,omitempty"`
	Reserves            map[string]quotaGrant `json:"reserves,omitempty"`
	UnlimitedOwners     map[string]bool       `json:"unlimitedOwners,omitempty"`
	BytesPerSecond      int64                 `json:"bytesPerSecond"`
	TotalBytesPerSecond int64                 `json:"totalBytesPerSecond"`
	BlockAll            bool                  `json:"blockAll"`
	BlockedOwners       map[string]bool       `json:"blockedOwners"`
}

type hostBucket struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

var hostPolicies = struct {
	sync.Mutex
	document        hostPolicyDocument
	buckets         map[string]*hostBucket
	frozen          map[string]bool
	admissions      map[string]int
	pendingDrain    int
	changed         chan struct{}
	recoveryFence   *quotaRecoveryFence
	recoveryInvalid bool
}{buckets: make(map[string]*hostBucket), frozen: make(map[string]bool), admissions: make(map[string]int), changed: make(chan struct{})}

var hostPolicyUpdates sync.Mutex
var errHostPolicyFenced = errors.New("host quota transition is pending")

const quotaFenceCapability = "remnacust-quota-fences-v1"
const coreIncarnationCapability = "remnacust-core-incarnation-v1"

// A PID can repeat after a container or process restart. This value changes
// once per core process and remains stable across native runtime generations.
var coreIncarnation = core.ProcessID()

func notifyHostPolicyLocked() {
	close(hostPolicies.changed)
	hostPolicies.changed = make(chan struct{})
}

func hostAdmissionKey(email string) string {
	_, host := hostPolicyIdentity(email)
	if host != "" {
		return "host:" + host
	}
	return "legacy"
}

type hostAdmissionTicket struct {
	key     string
	pending bool
}

func (ticket *hostAdmissionTicket) finishLocked() {
	if !ticket.pending {
		return
	}
	ticket.pending = false
	hostPolicies.admissions[ticket.key]--
	if hostPolicies.frozen[ticket.key] {
		hostPolicies.pendingDrain--
		if hostPolicies.pendingDrain == 0 {
			notifyHostPolicyLocked()
		}
	}
	if hostPolicies.admissions[ticket.key] == 0 {
		delete(hostPolicies.admissions, ticket.key)
	}
}
func (ticket *hostAdmissionTicket) finish() {
	if !ticket.pending {
		return
	}
	hostPolicies.Lock()
	ticket.finishLocked()
	hostPolicies.Unlock()
}

// Only affected hosts stop admitting new buffers. Existing admissions complete
// their journal flush and counter update before a new policy can acknowledge.
func changedHostAdmissions(previous, next hostPolicyDocument) map[string]bool {
	groups := make(map[string]bool)
	for key, value := range previous.Groups {
		if !reflect.DeepEqual(value, next.Groups[key]) {
			groups[key] = true
		}
	}
	for key, value := range next.Groups {
		if !reflect.DeepEqual(value, previous.Groups[key]) {
			groups[key] = true
		}
	}
	legacyChanged := previous.UserQuota != next.UserQuota || !reflect.DeepEqual(previous.LegacyGroups, next.LegacyGroups) || !reflect.DeepEqual(previous.LegacyOwners, next.LegacyOwners) || !reflect.DeepEqual(previous.LegacyQuotaMultipliers, next.LegacyQuotaMultipliers)
	for _, key := range append(append([]string(nil), previous.LegacyGroups...), next.LegacyGroups...) {
		legacyChanged = legacyChanged || groups[key]
	}
	modes := previous.Version != next.Version || previous.OfflineReserves != next.OfflineReserves
	affected := make(map[string]bool)
	for id, old := range previous.Hosts {
		value, exists := next.Hosts[id]
		old.destinations, value.destinations = nil, nil
		changed := modes || !exists || !reflect.DeepEqual(old, value) || (old.UserQuotaExempt && legacyChanged)
		for _, group := range old.Groups {
			changed = changed || groups[group]
		}
		if changed {
			affected["host:"+id] = true
		}
	}
	if modes || previous.Group != next.Group || previous.BlockAll != next.BlockAll || previous.BytesPerSecond != next.BytesPerSecond || !reflect.DeepEqual(previous.BlockedOwners, next.BlockedOwners) || !reflect.DeepEqual(previous.ProtectedInbounds, next.ProtectedInbounds) {
		affected["legacy"] = true
	}
	if legacyChanged {
		affected["legacy"] = true
	}
	return affected
}

func publishHostPolicy(next hostPolicyDocument) bool {
	if next.QuotaCheckpoint != nil && next.QuotaRegistrations != quotaRegistrationCapability {
		return false
	}
	if (next.QuotaRegistrations != "" || len(next.Registrations) != 0) && (next.QuotaRegistrations != quotaRegistrationCapability || next.OfflineReserves != quotaReserveCapability) {
		return false
	}
	if err := validateUnreservedUsage(next); err != nil {
		return false
	}
	if err := validateUserQuotaPolicy(next); err != nil {
		return false
	}
	hostPolicyUpdates.Lock()
	defer hostPolicyUpdates.Unlock()
	required, requirementErr := userQuotaRequired()
	if requirementErr != nil || (required && next.UserQuota != userQuotaCapability) {
		return false
	}
	hostPolicies.Lock()
	if next.Generation == hostPolicies.document.Generation || (hostPolicies.document.OfflineReserves == quotaReserveCapability && next.OfflineReserves != quotaReserveCapability) || (hostPolicies.document.UserQuota == userQuotaCapability && next.UserQuota != userQuotaCapability) {
		hostPolicies.Unlock()
		return false
	}
	affected := changedHostAdmissions(hostPolicies.document, next)
	hostPolicies.pendingDrain = 0
	for key := range affected {
		hostPolicies.frozen[key] = true
		hostPolicies.pendingDrain += hostPolicies.admissions[key]
	}
	for hostPolicies.pendingDrain > 0 {
		wake := hostPolicies.changed
		hostPolicies.Unlock()
		<-wake
		hostPolicies.Lock()
	}
	hostPolicies.Unlock()
	var err error
	if next.OfflineReserves == quotaReserveCapability {
		if next.UserQuota == userQuotaCapability {
			err = ensureUserQuotaRequired()
		}
		if err == nil {
			err = localQuota.reconcile(next, time.Now())
		}
	}
	if err == nil {
		for id, access := range next.Hosts {
			access.destinations = compileHostDestinations(access.Domains)
			next.Hosts[id] = access
		}
	}
	hostPolicies.Lock()
	if err == nil {
		hostPolicies.document = next
	}
	for key := range affected {
		delete(hostPolicies.frozen, key)
	}
	notifyHostPolicyLocked()
	hostPolicies.Unlock()
	return err == nil
}

// Identity comes from protocol authentication, never a client-supplied host header.
func hostPolicyIdentity(email string) (string, string) {
	parts := strings.Split(email, "~")
	if len(parts) != 1 && len(parts) != 2 && len(parts) != 3 {
		return "", ""
	}
	owner := parts[0]
	if owner == "" || owner[0] == '0' {
		return "", ""
	}
	for _, ch := range owner {
		if ch < '0' || ch > '9' {
			return "", ""
		}
	}
	hex := func(value string) bool {
		for _, ch := range value {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
				return false
			}
		}
		return true
	}
	if len(parts) > 1 && (len(parts[1]) != 24 || !hex(parts[1])) {
		return "", ""
	}
	if len(parts) == 3 {
		if len(parts[2]) != 33 || parts[2][0] != 'h' || !hex(parts[2][1:]) {
			return "", ""
		}
		return owner, parts[2][1:]
	}
	return owner, ""
}
func hostPolicyOwner(email string) string { owner, _ := hostPolicyIdentity(email); return owner }

var hostPolicyStart sync.Once
var hostPolicyStartError error

func startHostPolicy() error {
	// Node policy files belong to the Linux node runtime. Ordinary desktop and
	// mobile cores must not access server state or start a filesystem watcher.
	if runtime.GOOS != "linux" {
		return nil
	}
	_, policyExists := os.Stat(hostPolicyPath)
	if os.Getenv("REMNACUST_NODE_RUNTIME") != "1" && os.Getenv("XERA_CORE_MANAGER_VERSION") != "1" && policyExists != nil {
		if _, err := os.Lstat(userQuotaRequirementPath()); err != nil {
			return nil
		}
	}
	hostPolicyStart.Do(func() {
		// An old acknowledgement must not survive a PID reused by a new container.
		// Until this process publishes its policy, the node must see no valid ACK.
		if err := invalidateHostPolicyStatus(filepath.Join(filepath.Dir(hostPolicyPath), "host-policy.status.json")); err != nil {
			hostPolicyStartError = err
			return
		}
		// Load before accepting traffic; a stale limited policy remains fail-closed.
		if err := refreshRecoveryFence(quotaRecoveryFencePath, quotaRecoveryFenceStatusPath, true); err != nil {
			hostPolicyStartError = err
			return
		}
		loadHostPolicy()
		if err := validateQuotaCheckpointStartup(); err != nil {
			hostPolicyStartError = err
			return
		}
		if err := validateUserQuotaStartup(); err != nil {
			hostPolicyStartError = err
			return
		}
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for range tick.C {
				_ = refreshRecoveryFence(quotaRecoveryFencePath, quotaRecoveryFenceStatusPath, false)
				loadHostPolicy()
			}
		}()
	})
	return hostPolicyStartError
}

func invalidateHostPolicyStatus(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func loadHostPolicy() {
	data, err := os.ReadFile(hostPolicyPath)
	if err != nil {
		return
	} // never clear a known policy after a failed read
	var next hostPolicyDocument
	if json.Unmarshal(data, &next) != nil || (next.Version != hostPolicyCapability && next.Version != legacyHostPolicyCapability) ||
		next.Generation == "" || next.BytesPerSecond < 0 {
		return
	}
	if next.UserQuota == userQuotaCapability {
		userQuotaObserved.Store(true)
	}
	changed := publishHostPolicy(next)
	hostPolicies.Lock()
	for key, bucket := range hostPolicies.buckets {
		if time.Since(bucket.lastUsed) > 10*time.Minute {
			delete(hostPolicies.buckets, key)
		}
	}
	if time.Since(unreservedLastPrune) >= time.Minute {
		for manager := range unreservedManagers {
			pruneUnreservedUsageLocked(hostPolicies.document, manager)
		}
		unreservedLastPrune = time.Now()
	}
	hostPolicies.Unlock()
	if changed {
		status, _ := json.Marshal(map[string]any{"version": hostPolicyCapability, "destinationRules": destinationRulesCapability, "generation": next.Generation, "pid": os.Getpid(), "quotaReserves": quotaReserveCapability, "quotaRegistrations": quotaRegistrationCapability, "quotaWatermarks": quotaWatermarkCapability, "quotaFences": quotaFenceCapability, "coreIncarnation": coreIncarnationCapability, "coreProcessId": coreIncarnation, "userQuotaReserves": userQuotaCapability, "unreservedUsage": quotausage.Capability})
		tmp := hostPolicyPath + ".status.tmp"
		if os.WriteFile(tmp, status, 0600) == nil {
			_ = os.Rename(tmp, filepath.Join(filepath.Dir(hostPolicyPath), "host-policy.status.json"))
		}
	}
}

// Compiled once per policy generation, never resolve user supplied domains here.
type hostDestinations struct {
	domains   map[string]bool
	addresses map[netip.Addr]bool
	prefixes  []netip.Prefix
}

func compileHostDestinations(rules []string) *hostDestinations {
	result := &hostDestinations{domains: map[string]bool{}, addresses: map[netip.Addr]bool{}}
	for _, rule := range rules {
		if prefix, err := netip.ParsePrefix(rule); err == nil {
			if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			result.prefixes = append(result.prefixes, prefix.Masked())
		} else if address, err := netip.ParseAddr(rule); err == nil {
			result.addresses[address.Unmap()] = true
		} else if !strings.ContainsAny(rule, "/:[]") {
			ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(strings.ToLower(rule), "."))
			if err == nil && ascii != "" {
				result.domains[ascii] = true
			}
		}
	}
	return result
}
func (rules *hostDestinations) matches(target string) bool {
	target = strings.TrimSuffix(strings.ToLower(target), ".")
	if address, err := netip.ParseAddr(target); err == nil {
		address = address.Unmap()
		if rules.addresses[address] {
			return true
		}
		for _, prefix := range rules.prefixes {
			if prefix.Contains(address) {
				return true
			}
		}
		return false
	}
	if strings.ContainsAny(target, "/:[]") {
		return false
	}
	ascii, err := idna.Lookup.ToASCII(target)
	if err != nil {
		return false
	}
	for domain := ascii; domain != ""; {
		if rules.domains[domain] {
			return true
		}
		dot := strings.IndexByte(domain, '.')
		if dot < 0 {
			break
		}
		domain = domain[dot+1:]
	}
	return false
}

// Called with the policy mutex held. Legacy keys cannot enter a protected inbound.
func selectedHostLimits(ctx context.Context, email string, policy hostPolicyDocument) (map[string]hostLimitPolicy, error) {
	denied := errors.New("host access or quota denied traffic")
	if instance := core.FromContext(ctx); instance != nil && instance.RequiresUserQuota() && policy.UserQuota != userQuotaCapability {
		return nil, denied
	}
	owner, host := hostPolicyIdentity(email)
	if host == "" {
		if inbound := session.InboundFromContext(ctx); inbound != nil {
			if instance := core.FromContext(ctx); instance != nil && instance.RequiresHostPolicy(inbound.Tag) {
				return nil, denied
			}
			for _, tag := range policy.ProtectedInbounds {
				if tag == inbound.Tag {
					return nil, denied
				}
			}
		}
		return selectedLegacyLimits(owner, policy)
	}
	access, ok := policy.Hosts[host]
	inbound := session.InboundFromContext(ctx)
	if !ok || !access.AllowedIdentities[email] || inbound == nil || inbound.Tag != access.InboundTag || (policy.OfflineReserves != quotaReserveCapability && time.Now().UnixMilli() >= policy.ExpiresAt) {
		return nil, denied
	}
	if policy.OfflineReserves == quotaReserveCapability {
		expires, known := access.IdentityExpiresAt[email]
		if !known || (expires != 0 && time.Now().UnixMilli() >= expires) {
			return nil, denied
		}
	}
	if access.DomainMode != "" && access.DomainMode != "OFF" {
		out := session.OutboundsFromContext(ctx)
		if len(out) == 0 || out[len(out)-1].Target.Address == nil {
			return nil, denied
		}
		target := strings.TrimSuffix(strings.ToLower(out[len(out)-1].Target.Address.String()), ".")
		matches := false
		destinations := access.destinations
		if destinations == nil {
			destinations = compileHostDestinations(access.Domains)
		}
		matches = destinations.matches(target)
		if (access.DomainMode == "ALLOW_ONLY" && !matches) || (access.DomainMode == "DENY" && matches) {
			return nil, denied
		}
	}
	result := make(map[string]hostLimitPolicy, len(access.Groups))
	for _, key := range access.Groups {
		g, exists := policy.Groups[key]
		if !exists {
			return nil, denied
		}
		result[key] = g
	}
	return result, nil
}
func waitHostPolicy(ctx context.Context, email, direction string, size int) error {
	return waitHostPolicyAndAccount(ctx, email, direction, size, nil)
}

// Accounting is part of admission. Once a blocking policy is acknowledged, a
// later stats reset cannot miss bytes admitted immediately before that fence.
func waitHostPolicyAndAccount(ctx context.Context, email, direction string, size int, account func(), usage ...*unreservedUsage) error {
	for {
		err := admitHostPolicy(ctx, email, direction, size, account, usage...)
		if !errors.Is(err, errHostPolicyFenced) {
			return err
		}
		if err = waitForHostPolicy(ctx, email); err != nil {
			return err
		}
	}
}

func hostAdmissionFenced(email string, groups map[string]hostLimitPolicy) bool {
	if hostPolicyOwner(email) != "" && (hostPolicies.recoveryInvalid || (hostPolicies.recoveryFence != nil && hostPolicies.recoveryFence.State == "FENCED")) {
		return true
	}
	if hostPolicies.frozen[hostAdmissionKey(email)] {
		return true
	}
	owner := hostPolicyOwner(email)
	for _, group := range groups {
		if group.FenceAll || group.FencedOwners[owner] {
			return true
		}
	}
	// Recovery bypasses the finite USER grant, not the admission barrier that
	// establishes its gross debt before creating or redistributing that grant.
	_, host := hostPolicyIdentity(email)
	if host != "" && hostPolicies.document.Hosts[host].UserQuotaExempt {
		for _, key := range hostPolicies.document.LegacyGroups {
			group := hostPolicies.document.Groups[key]
			if group.FenceAll || group.FencedOwners[owner] {
				return true
			}
		}
	}
	return false
}

func waitForHostPolicy(ctx context.Context, email string) error {
	hostPolicies.Lock()
	groups, err := selectedHostLimits(ctx, email, hostPolicies.document)
	owner, host := hostPolicyIdentity(email)
	if err == nil {
		for _, group := range groups {
			if group.BlockAll || group.BlockedOwners[owner] {
				err = errors.New("host quota denied traffic")
			}
		}
	}
	if err != nil || !hostAdmissionFenced(email, groups) {
		hostPolicies.Unlock()
		return err
	}
	wake := hostPolicies.changed
	var expiry int64
	if host != "" {
		if hostPolicies.document.OfflineReserves == quotaReserveCapability {
			expiry = hostPolicies.document.Hosts[host].IdentityExpiresAt[email]
		} else {
			expiry = hostPolicies.document.ExpiresAt
		}
	} else if len(hostPolicies.document.LegacyGroups) > 0 {
		expiry = hostPolicies.document.LegacyOwners[owner].ExpiresAt
	} else if hostPolicies.document.Group != "" {
		expiry = hostPolicies.document.ExpiresAt
	}
	hostPolicies.Unlock()
	var stopped <-chan struct{}
	if instance := core.FromContext(ctx); instance != nil {
		stopped = instance.RuntimeDone()
	}
	var timer *time.Timer
	var deadline <-chan time.Time
	if expiry > 0 {
		timer = time.NewTimer(time.Until(time.UnixMilli(expiry)))
		deadline = timer.C
		defer timer.Stop()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stopped:
		return errors.New("Xray instance is closing")
	case <-wake:
		return nil
	case <-deadline:
		return errors.New("host identity expired while waiting for quota")
	}
}

func admitHostPolicy(ctx context.Context, email, direction string, size int, account func(), usage ...*unreservedUsage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, quotaHost := hostPolicyIdentity(email)
	chargeSize := size
	hostPolicies.Lock()
	policy := hostPolicies.document
	groups, err := selectedHostLimits(ctx, email, policy)
	if err != nil {
		hostPolicies.Unlock()
		return err
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	buckets := make([]*rate.Limiter, 0, len(keys)*2)
	for _, key := range keys {
		group := groups[key]
		if group.BlockAll || group.BlockedOwners[owner] {
			hostPolicies.Unlock()
			return errors.New("host quota denied traffic")
		}
		for _, limit := range []struct {
			scope string
			speed int64
		}{{owner, group.BytesPerSecond}, {"all", group.TotalBytesPerSecond}} {
			if limit.speed <= 0 {
				continue
			}
			// Upload and download share one budget, as do every device and connection.
			bucketKey := key + "/" + limit.scope
			bucket := hostPolicies.buckets[bucketKey]
			if bucket == nil {
				bucket = &hostBucket{limiter: rate.NewLimiter(rate.Limit(limit.speed), policyBurst)}
				hostPolicies.buckets[bucketKey] = bucket
			} else if bucket.limiter.Limit() != rate.Limit(limit.speed) {
				bucket.limiter.SetLimit(rate.Limit(limit.speed))
			}
			bucket.lastUsed = time.Now()
			buckets = append(buckets, bucket.limiter)
		}
	}
	if hostAdmissionFenced(email, groups) {
		hostPolicies.Unlock()
		return errHostPolicyFenced
	}
	hostPolicies.Unlock()
	for size > 0 {
		n := min(size, policyBurst)
		for _, bucket := range buckets {
			deadline := time.UnixMilli(policy.ExpiresAt)
			if policy.OfflineReserves == quotaReserveCapability {
				deadline = time.Now().Add(24 * time.Hour)
			}
			waitCtx, cancel := context.WithDeadline(ctx, deadline)
			err := bucket.WaitN(waitCtx, n)
			cancel()
			if err != nil {
				return err
			}
		}
		size -= n
	}
	// Check revocations and changed group membership after every wait.
	var debits []quotaDebit
	ticket := &hostAdmissionTicket{}
	err = func() error {
		hostPolicies.Lock()
		defer hostPolicies.Unlock()
		current, err := selectedHostLimits(ctx, email, hostPolicies.document)
		if err != nil {
			return err
		}
		if hostAdmissionFenced(email, current) {
			return errHostPolicyFenced
		}
		for key, group := range current {
			if group.BlockAll || group.BlockedOwners[owner] {
				return errors.New("host quota denied traffic")
			}
			if previous, exists := groups[key]; !exists || (group.BytesPerSecond > 0 && (previous.BytesPerSecond == 0 || group.BytesPerSecond < previous.BytesPerSecond)) || (group.TotalBytesPerSecond > 0 && (previous.TotalBytesPerSecond == 0 || group.TotalBytesPerSecond < previous.TotalBytesPerSecond)) {
				return errors.New("host policy changed; reconnect")
			}
		}
		debits = make([]quotaDebit, 0, len(current))
		for key, group := range current {
			if !group.ReserveRequired {
				continue
			}
			if hostPolicies.document.OfflineReserves != quotaReserveCapability {
				return errors.New("quota capability missing")
			}
			if group.UnlimitedOwners[owner] {
				continue
			}
			grant, exists := group.Reserves[owner]
			factor, scale, factorErr := quotaChargeFactor(hostPolicies.document, quotaHost, key, group, chargeSize)
			if !exists || factorErr != nil {
				return errors.New("local quota reserve unavailable")
			}
			units := int64(chargeSize) * factor
			debits = append(debits, quotaDebit{Grant: grant, Group: key, Owner: owner, Cost: units / scale, Fraction: units % scale, Scale: scale})
		}
		ticket.key, ticket.pending = hostAdmissionKey(email), true
		hostPolicies.admissions[ticket.key]++
		return nil
	}()
	if err != nil {
		return err
	}
	defer ticket.finish()
	// Disk flushes must not hold the policy mutex for unrelated ports/users.
	if err := localQuota.debit(debits, time.Now()); err != nil {
		return err
	}
	hostPolicies.Lock()
	defer hostPolicies.Unlock()
	defer ticket.finishLocked()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := selectedHostLimits(ctx, email, hostPolicies.document)
	if err != nil {
		return err
	}
	for key, group := range current {
		if group.BlockAll || group.BlockedOwners[owner] {
			return errors.New("host quota denied traffic")
		}
		if !group.ReserveRequired || group.UnlimitedOwners[owner] {
			continue
		}
		matched := false
		for _, d := range debits {
			if d.Group != key {
				continue
			}
			grant, granted := group.Reserves[owner]
			factor, scale, factorErr := quotaChargeFactor(hostPolicies.document, quotaHost, key, group, chargeSize)
			if factorErr != nil || quotaScale(d.Scale) != scale {
				return errors.New("invalid quota multiplier after policy change")
			}
			units := int64(chargeSize) * factor
			covered := units/scale < d.Cost || (units/scale == d.Cost && units%scale <= d.Fraction)
			matched = granted && grant.ID == d.Grant.ID && grant.Instance == d.Grant.Instance && covered
			break
		}
		if !matched {
			return errors.New("host policy changed while flushing quota; reconnect")
		}
	}
	var tracker *unreservedUsage
	if len(usage) > 0 {
		tracker = usage[0]
	}
	if err := tracker.accountLocked(hostPolicies.document, quotaHost, owner, direction, current, chargeSize); err != nil {
		return err
	}
	if account != nil {
		account()
	}
	return nil
}

type hostPolicyWriter struct {
	usage *unreservedUsage
	buf.Writer
	ctx              context.Context
	email, direction string
	counter          stats.Counter
	cancel           context.CancelFunc
}

func (w *hostPolicyWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	for i, buffer := range mb {
		if err := waitHostPolicyAndAccount(w.ctx, w.email, w.direction, int(buffer.Len()), func() {
			if w.counter != nil {
				w.counter.Add(int64(buffer.Len()))
			}
		}, w.usage); err != nil {
			buf.ReleaseMulti(mb[i:])
			return err
		}
		if err := w.Writer.WriteMultiBuffer(buf.MultiBuffer{buffer}); err != nil {
			buf.ReleaseMulti(mb[i+1:])
			return err
		}
	}
	return nil
}
func (w *hostPolicyWriter) Close() error { return common.Close(w.Writer) }
func (w *hostPolicyWriter) Interrupt() {
	if w.cancel != nil {
		w.cancel()
	}
	common.Interrupt(w.Writer)
}

type hostPolicyReader struct {
	usage *unreservedUsage
	buf.Reader
	ctx     context.Context
	email   string
	counter stats.Counter
	cancel  context.CancelFunc
}

func (r *hostPolicyReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if policyErr := waitHostPolicyAndAccount(r.ctx, r.email, "up", int(mb.Len()), func() {
		if r.counter != nil {
			r.counter.Add(int64(mb.Len()))
		}
	}, r.usage); policyErr != nil {
		buf.ReleaseMulti(mb)
		return nil, policyErr
	}
	return mb, err
}
func (r *hostPolicyReader) Interrupt() {
	if r.cancel != nil {
		r.cancel()
	}
	common.Interrupt(r.Reader)
}

func applyHostPolicy(ctx context.Context, inbound, outbound *transport.Link) {
	metadata := session.InboundFromContext(ctx)
	if metadata == nil || metadata.User == nil || metadata.User.Email == "" {
		return
	}
	// Prevent Vision/splice from bypassing the authenticated traffic wrappers.
	// Install for all authenticated links so policy changes cover existing streams.
	metadata.CanSpliceCopy = 3
	email := metadata.User.Email
	ctx, cancel := context.WithCancel(ctx)
	usage := newUnreservedUsage(ctx)
	wrapWriter := func(writer buf.Writer, direction string) buf.Writer {
		var counter stats.Counter
		if measured, ok := writer.(*SizeStatWriter); ok {
			counter, writer = measured.Counter, measured.Writer
		}
		return &hostPolicyWriter{usage: usage, Writer: writer, ctx: ctx, email: email, direction: direction, counter: counter, cancel: cancel}
	}
	if outbound != nil {
		inbound.Writer = wrapWriter(inbound.Writer, "up")
		outbound.Writer = wrapWriter(outbound.Writer, "down")
	} else {
		// Keep TimeoutWrapperReader outermost; dispatch may rely on this interface.
		if reader, ok := inbound.Reader.(*buf.TimeoutWrapperReader); ok {
			reader.Reader = &hostPolicyReader{usage: usage, Reader: reader.Reader, ctx: ctx, email: email, counter: reader.Counter, cancel: cancel}
			reader.Counter = nil
		} else if inbound.Reader != nil {
			inbound.Reader = &hostPolicyReader{usage: usage, Reader: inbound.Reader, ctx: ctx, email: email, cancel: cancel}
		}
		inbound.Writer = wrapWriter(inbound.Writer, "down")
	}
}
