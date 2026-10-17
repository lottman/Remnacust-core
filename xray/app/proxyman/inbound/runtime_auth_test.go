package inbound

import (
	"context"
	"sync"
	"testing"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/vless"
)

type runtimeAuthProxy struct {
	users map[string]*protocol.MemoryUser
}

// Embed an uncalled inbound for the protocol-independent authorization gate.
type runtimeAuthManagedProxy struct {
	runtimeAuthProxy
	proxy.Inbound
}

func (p runtimeAuthProxy) AddUser(_ context.Context, user *protocol.MemoryUser) error {
	p.users[user.Email] = user
	return nil
}
func (p runtimeAuthProxy) RemoveUser(_ context.Context, email string) error {
	delete(p.users, email)
	return nil
}
func (p runtimeAuthProxy) GetUser(_ context.Context, email string) *protocol.MemoryUser {
	return p.users[email]
}
func (p runtimeAuthProxy) GetUsers(context.Context) []*protocol.MemoryUser {
	var users []*protocol.MemoryUser
	for _, user := range p.users {
		users = append(users, user)
	}
	return users
}
func (p runtimeAuthProxy) GetUsersCount(context.Context) int64 { return int64(len(p.users)) }

func TestRuntimeCachedAuthenticationIsRevalidatedAfterCredentialRemoval(t *testing.T) {
	user, err := (&protocol.User{Email: "cached-stream-user", Account: serial.ToTypedMessage(&vless.Account{Id: "84f64590-a13f-4f1b-82bf-3d3da3fc2123"})}).ToMemoryUser()
	if err != nil {
		t.Fatal(err)
	}
	proxy := runtimeAuthManagedProxy{runtimeAuthProxy: runtimeAuthProxy{users: map[string]*protocol.MemoryUser{user.Email: user}}}
	dispatcher := &runtimeAuthDispatcher{proxy: proxy, barrier: new(sync.RWMutex)}
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "PORT-A", User: user})
	if err := dispatcher.authorize(ctx); err != nil {
		t.Fatal(err)
	}
	delete(proxy.users, user.Email)
	if err := dispatcher.authorize(ctx); err == nil {
		t.Fatal("cached transport identity bypassed deletion")
	}
	changed, err := (&protocol.User{Email: user.Email, Account: serial.ToTypedMessage(&vless.Account{Id: "8541d79a-b6ca-4006-a842-2cf688317f5c"})}).ToMemoryUser()
	if err != nil {
		t.Fatal(err)
	}
	proxy.users[user.Email] = changed
	if err := dispatcher.authorize(ctx); err == nil {
		t.Fatal("cached stream accepted replaced credentials")
	}
	proxy.users[user.Email] = user
	if err := dispatcher.authorize(ctx); err != nil {
		t.Fatal("re-added native credential was not accepted", err)
	}
}
