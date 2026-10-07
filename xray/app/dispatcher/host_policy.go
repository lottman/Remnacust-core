//go:build !wasm

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/session"
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
	Version           string                      `json:"version"`
	Generation        string                      `json:"generation"`
	ExpiresAt         int64                       `json:"expiresAt"`
	ProtectedInbounds []string                    `json:"protectedInbounds"`
	Hosts             map[string]hostAccessPolicy `json:"hosts"`
	Groups            map[string]hostLimitPolicy  `json:"groups"`
	Group             string                      `json:"group"`
	BytesPerSecond    int64                       `json:"bytesPerSecond"`
	BlockedOwners     map[string]bool             `json:"blockedOwners"`
	BlockAll          bool                        `json:"blockAll"`
}

type hostAccessPolicy struct {
	InboundTag        string          `json:"inboundTag"`
	AllowedIdentities map[string]bool `json:"allowedIdentities"`
	Groups            []string        `json:"groups"`
	DomainMode        string          `json:"domainMode"`
	Domains           []string        `json:"domains"`
	destinations      *hostDestinations
}
type hostLimitPolicy struct {
	BytesPerSecond      int64           `json:"bytesPerSecond"`
	TotalBytesPerSecond int64           `json:"totalBytesPerSecond"`
	BlockAll            bool            `json:"blockAll"`
	BlockedOwners       map[string]bool `json:"blockedOwners"`
}

type hostBucket struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

var hostPolicies = struct {
	sync.Mutex
	document hostPolicyDocument
	buckets  map[string]*hostBucket
}{buckets: make(map[string]*hostBucket)}

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

func init() {
	// Load before accepting traffic; a stale limited policy remains fail-closed.
	loadHostPolicy()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for range tick.C {
			loadHostPolicy()
		}
	}()
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
	hostPolicies.Lock()
	changed := next.Generation != hostPolicies.document.Generation
	if changed {
		for id, access := range next.Hosts {
			access.destinations = compileHostDestinations(access.Domains)
			next.Hosts[id] = access
		}
		hostPolicies.document = next
	}
	for key, bucket := range hostPolicies.buckets {
		if time.Since(bucket.lastUsed) > 10*time.Minute {
			delete(hostPolicies.buckets, key)
		}
	}
	hostPolicies.Unlock()
	if changed {
		status, _ := json.Marshal(map[string]any{"version": hostPolicyCapability, "destinationRules": destinationRulesCapability, "generation": next.Generation, "pid": os.Getpid()})
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
	owner, host := hostPolicyIdentity(email)
	if host == "" {
		if inbound := session.InboundFromContext(ctx); inbound != nil {
			for _, tag := range policy.ProtectedInbounds {
				if tag == inbound.Tag {
					return nil, denied
				}
			}
		}
		if policy.Group == "" {
			return nil, nil
		}
		if owner == "" || time.Now().UnixMilli() >= policy.ExpiresAt {
			return nil, denied
		}
		return map[string]hostLimitPolicy{policy.Group: {BytesPerSecond: policy.BytesPerSecond, BlockAll: policy.BlockAll, BlockedOwners: policy.BlockedOwners}}, nil
	}
	access, ok := policy.Hosts[host]
	inbound := session.InboundFromContext(ctx)
	if !ok || !access.AllowedIdentities[email] || inbound == nil || inbound.Tag != access.InboundTag || time.Now().UnixMilli() >= policy.ExpiresAt {
		return nil, denied
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
	owner := hostPolicyOwner(email)
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
	hostPolicies.Unlock()
	for size > 0 {
		n := min(size, policyBurst)
		for _, bucket := range buckets {
			waitCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(policy.ExpiresAt))
			err := bucket.WaitN(waitCtx, n)
			cancel()
			if err != nil {
				return err
			}
		}
		size -= n
	}
	// Check revocations and changed group membership after every wait.
	hostPolicies.Lock()
	defer hostPolicies.Unlock()
	current, err := selectedHostLimits(ctx, email, hostPolicies.document)
	if err != nil {
		return err
	}
	for key, group := range current {
		if group.BlockAll || group.BlockedOwners[owner] {
			return errors.New("host quota denied traffic")
		}
		if previous, exists := groups[key]; !exists || (group.BytesPerSecond > 0 && (previous.BytesPerSecond == 0 || group.BytesPerSecond < previous.BytesPerSecond)) || (group.TotalBytesPerSecond > 0 && (previous.TotalBytesPerSecond == 0 || group.TotalBytesPerSecond < previous.TotalBytesPerSecond)) {
			return errors.New("host policy changed; reconnect")
		}
	}
	return nil
}

type hostPolicyWriter struct {
	buf.Writer
	ctx              context.Context
	email, direction string
}

func (w *hostPolicyWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	for i, buffer := range mb {
		if err := waitHostPolicy(w.ctx, w.email, w.direction, int(buffer.Len())); err != nil {
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
func (w *hostPolicyWriter) Interrupt()   { common.Interrupt(w.Writer) }

type hostPolicyReader struct {
	buf.Reader
	ctx   context.Context
	email string
}

func (r *hostPolicyReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if policyErr := waitHostPolicy(r.ctx, r.email, "up", int(mb.Len())); policyErr != nil {
		buf.ReleaseMulti(mb)
		return nil, policyErr
	}
	return mb, err
}
func (r *hostPolicyReader) Interrupt() { common.Interrupt(r.Reader) }

func applyHostPolicy(ctx context.Context, inbound, outbound *transport.Link) {
	metadata := session.InboundFromContext(ctx)
	if metadata == nil || metadata.User == nil || metadata.User.Email == "" {
		return
	}
	// Prevent Vision/splice from bypassing the authenticated traffic wrappers.
	// Install for all authenticated links so policy changes cover existing streams.
	metadata.CanSpliceCopy = 3
	email := metadata.User.Email
	if outbound != nil {
		inbound.Writer = &hostPolicyWriter{inbound.Writer, ctx, email, "up"}
		outbound.Writer = &hostPolicyWriter{outbound.Writer, ctx, email, "down"}
	} else {
		// Keep TimeoutWrapperReader outermost; dispatch may rely on this interface.
		if reader, ok := inbound.Reader.(*buf.TimeoutWrapperReader); ok {
			reader.Reader = &hostPolicyReader{reader.Reader, ctx, email}
		} else if inbound.Reader != nil {
			inbound.Reader = &hostPolicyReader{inbound.Reader, ctx, email}
		}
		inbound.Writer = &hostPolicyWriter{inbound.Writer, ctx, email, "down"}
	}
}
