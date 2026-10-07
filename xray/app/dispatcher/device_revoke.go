//go:build !wasm

package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/transport"
)

type userLink struct {
	inbound  *transport.Link
	outbound *transport.Link
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
	common.Close(link.Writer)
}

func trackUserLink(ctx context.Context, email string, inbound, outbound *transport.Link) {
	if email == "" {
		return
	}
	entry := &userLink{inbound: inbound, outbound: outbound}
	activeUserLinks.Lock()
	if until, blocked := activeUserLinks.revoked[email]; blocked && time.Now().Before(until) {
		activeUserLinks.Unlock()
		interruptLink(inbound)
		interruptLink(outbound)
		return
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
		interruptLink(link.inbound)
		interruptLink(link.outbound)
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
