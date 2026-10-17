package mux

import (
	"context"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/vless"
	"testing"
)

func TestXUDPAssociationCannotCrossUsersPortsOrGenerations(t *testing.T) {
	identity := func(email, id string) *protocol.MemoryUser {
		user, err := (&protocol.User{Email: email, Account: serial.ToTypedMessage(&vless.Account{Id: id})}).ToMemoryUser()
		if err != nil {
			t.Fatal(err)
		}
		return user
	}
	first := identity("first", "e3e80750-8209-4796-8c55-af86edc00b1b")
	second := identity("second", "867d87df-0f9e-4311-a4cc-55c2fda17f57")
	root, other := new(core.Instance), new(core.Instance)
	makeContext := func(instance *core.Instance, tag string, port net.Port, user *protocol.MemoryUser) context.Context {
		ctx := context.WithValue(context.Background(), core.XrayKey(1), instance)
		return session.ContextWithInbound(ctx, &session.Inbound{Tag: tag, Gateway: net.TCPDestination(net.LocalHostIP, port), User: user})
	}
	id := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	key := xudpSessionKey(makeContext(root, "same", 443, first), id)
	if key != xudpSessionKey(makeContext(root, "same", 443, first), id) {
		t.Fatal("same authenticated association could not resume")
	}
	for _, ctx := range []context.Context{makeContext(root, "other", 443, first), makeContext(root, "same", 8443, first), makeContext(root, "same", 443, second), makeContext(other, "same", 443, first), makeContext(root, "same", 443, identity(first.Email, "f9a8cc28-c19c-42bf-9837-4498e59c6747"))} {
		if key == xudpSessionKey(ctx, id) {
			t.Fatal("client GlobalID crossed an authorization or routing boundary")
		}
	}
}
