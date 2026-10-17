//go:build !wasm

package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport"
)

type userLink struct {
	inbound  *transport.Link
	outbound *transport.Link
	conn     net.Conn
	tag      string
	profile  *sync.RWMutex
}

func (link *userLink) interrupt() {
	// Splice can bypass the pipes; close only this authenticated connection.
	if link.conn != nil {
		_ = link.conn.Close()
	}
	interruptLink(link.inbound)
	interruptLink(link.outbound)
}

var activeUserLinks = struct {
	sync.Mutex
	byEmail map[string]map[*userLink]struct{}
	revoked map[string]time.Time
}{
	byEmail: make(map[string]map[*userLink]struct{}),
	revoked: make(map[string]time.Time),
}

func interruptLink(link *transport.Link) {
	if link == nil {
		return
	}
	common.Interrupt(link.Reader)
	common.Interrupt(link.Writer)
}

func trackUserLink(ctx context.Context, email string, inbound, outbound *transport.Link) bool {
	if email == "" {
		return true
	}
	entry := &userLink{inbound: inbound, outbound: outbound}
	if instance := core.FromContext(ctx); instance != nil {
		entry.profile = instance.RuntimeAccess()
	}
	if metadata := session.InboundFromContext(ctx); metadata != nil {
		entry.conn = metadata.Conn
		entry.tag = metadata.Tag
	}
	if admission := session.AdmissionFromContext(ctx); admission != nil {
		release, err := admission(ctx)
		if err != nil {
			entry.interrupt()
			return false
		}
		if release != nil {
			defer release()
		}
	}
	activeUserLinks.Lock()
	if denied := retiredProfileRevocations[entry.profile]; denied != nil && denied[email] {
		activeUserLinks.Unlock()
		entry.interrupt()
		return false
	}
	if until, blocked := activeUserLinks.revoked[email]; blocked && time.Now().Before(until) {
		activeUserLinks.Unlock()
		entry.interrupt()
		return false
	}
	if activeUserLinks.byEmail[email] == nil {
		activeUserLinks.byEmail[email] = make(map[*userLink]struct{})
	}
	activeUserLinks.byEmail[email][entry] = struct{}{}
	activeUserLinks.Unlock()
	context.AfterFunc(ctx, func() {
		activeUserLinks.Lock()
		delete(activeUserLinks.byEmail[email], entry)
		if len(activeUserLinks.byEmail[email]) == 0 {
			delete(activeUserLinks.byEmail, email)
		}
		activeUserLinks.Unlock()
	})
	return true
}

// RevokeUserLinks closes only the Xray links authenticated as this email.
// It never destroys sockets based on the client's public IP.
func RevokeUserLinks(email string) {
	activeUserLinks.Lock()
	until := time.Now().Add(2 * time.Minute)
	activeUserLinks.revoked[email] = until
	links := make([]*userLink, 0, len(activeUserLinks.byEmail[email]))
	for link := range activeUserLinks.byEmail[email] {
		links = append(links, link)
	}
	activeUserLinks.Unlock()
	for _, link := range links {
		link.interrupt()
	}
	time.AfterFunc(2*time.Minute, func() {
		activeUserLinks.Lock()
		if activeUserLinks.revoked[email] == until {
			delete(activeUserLinks.revoked, email)
		}
		activeUserLinks.Unlock()
	})
}

// AllowUserLinks clears the revocation guard after a successful re-add.
func AllowUserLinks(email string) {
	activeUserLinks.Lock()
	delete(activeUserLinks.revoked, email)
	activeUserLinks.Unlock()
}

// A profile's credential change cannot interrupt the same identity on another port.
func RevokeInboundUserLinks(tag, email string) {
	revokeInboundUserLinks(nil, tag, email)
}

// Inbound tags are local to a profile. Generations share its admission barrier,
// which remains stable while old sessions drain and a new graph is installed.
func RevokeProfileInboundUserLinks(profile *core.Instance, tag, email string) {
	if profile == nil {
		return
	}
	for _, instance := range profile.RelatedManagedProfiles() {
		if instance.RetiredProfile() {
			denyRetiredProfileUser(instance, email)
		}
		if instance.RuntimeAccess() == profile.RuntimeAccess() {
			revokeInboundUserLinks(instance.RuntimeAccess(), tag, email)
			continue
		}
		// A replacement may rename its inbound. The binding identity, rather
		// than today's local tag, connects that user's retired authenticated links.
		for _, inbound := range instance.Configuration().Inbound {
			if inbound != nil {
				revokeInboundUserLinks(instance.RuntimeAccess(), inbound.Tag, email)
			}
		}
	}
}

// MarkRetiredProfileUserDenied only changes admission, so a topology commit can
// call it under its barriers without closing streams or current credentials.
func MarkRetiredProfileUserDenied(profile *core.Instance, email string) {
	if profile == nil {
		return
	}
	for _, instance := range profile.RelatedManagedProfiles() {
		if instance.RetiredProfile() {
			denyRetiredProfileUser(instance, email)
		}
	}
}

// A replacement credential may already be using the new graph. Revoke only
// retired graphs; the same email on the new graph must stay connected.
func RevokeRetiredProfileUserLinks(profile *core.Instance, email string) {
	if profile == nil {
		return
	}
	for _, instance := range profile.RelatedManagedProfiles() {
		RevokeRetiredInstanceUserLinks(instance, email)
	}
}

func RevokeRetiredInstanceUserLinks(instance *core.Instance, email string) {
	if instance == nil || !instance.RetiredProfile() {
		return
	}
	denyRetiredProfileUser(instance, email)
	for _, inbound := range instance.Configuration().Inbound {
		if inbound != nil {
			revokeInboundUserLinks(instance.RuntimeAccess(), inbound.Tag, email)
		}
	}
}

// A retired transport can outlive its credentials. Keep a profile-local deny
// record until that graph fully closes, so cached HTTP/2 and QUIC streams cannot
// authenticate again against its old native user store after a scoped removal.
var retiredProfileRevocations = make(map[*sync.RWMutex]map[string]bool) // guarded by activeUserLinks

func denyRetiredProfileUser(instance *core.Instance, email string) {
	if email == "" {
		return
	}
	scope := instance.RuntimeAccess()
	done := instance.RuntimeDone()
	activeUserLinks.Lock()
	denied := retiredProfileRevocations[scope]
	watch := denied == nil
	if watch {
		denied = make(map[string]bool)
		retiredProfileRevocations[scope] = denied
	}
	denied[email] = true
	activeUserLinks.Unlock()
	if watch {
		go func() {
			<-done
			// RuntimeDone marks the beginning of shutdown. Wait for native workers
			// to finish before removing the deny record, avoiding a closing race.
			_ = instance.Close()
			activeUserLinks.Lock()
			delete(retiredProfileRevocations, scope)
			activeUserLinks.Unlock()
		}()
	}
}

func revokeInboundUserLinks(profile *sync.RWMutex, tag, email string) {
	activeUserLinks.Lock()
	var links []*userLink
	for link := range activeUserLinks.byEmail[email] {
		if link.tag == tag && (profile == nil || link.profile == profile) {
			links = append(links, link)
		}
	}
	activeUserLinks.Unlock()
	for _, link := range links {
		link.interrupt()
	}
}
