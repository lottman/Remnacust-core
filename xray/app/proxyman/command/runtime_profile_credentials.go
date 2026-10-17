//go:build !wasm

package command

import (
	"context"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/proxy"
	"google.golang.org/protobuf/proto"
)

type profileCredentialRevocation struct {
	instance *core.Instance
	email    string
}

func profileNativeCredentials(ctx context.Context, instance *core.Instance) map[string][]*protocol.User {
	users := make(map[string][]*protocol.User)
	manager, ok := instance.GetFeature(inbound.ManagerType()).(inbound.Manager)
	if !ok {
		return users
	}
	for _, handler := range manager.ListHandlers(ctx) {
		server, err := getInbound(handler)
		if err != nil {
			continue
		}
		accounts, ok := server.(proxy.UserManager)
		if !ok {
			continue
		}
		for _, user := range accounts.GetUsers(ctx) {
			if user == nil || user.Email == "" {
				continue
			}
			users[user.Email] = append(users[user.Email], protocol.ToProtoUser(user))
		}
	}
	return users
}

func profileTopologyCredentialRevocations(ctx context.Context, owner *core.Instance, previous []core.ProfileBinding, next map[string]*core.Instance) []profileCredentialRevocation {
	var revoked []profileCredentialRevocation
	for _, binding := range previous {
		old := owner.ManagedProfile(binding.BindingID)
		current := next[binding.BindingID]
		// Removing a port drains its transport; an unrelated replacement binding
		// must not interrupt it just because the old binding disappeared.
		if old == nil || current == nil || old == current {
			continue
		}
		before, after := profileNativeCredentials(ctx, old), profileNativeCredentials(ctx, current)
		for email, credentials := range before {
			changed := false
			for _, credential := range credentials {
				retained := false
				for _, candidate := range after[email] {
					if proto.Equal(credential.Account, candidate.Account) {
						retained = true
						break
					}
				}
				if !retained {
					changed = true
					break
				}
			}
			if changed {
				revoked = append(revoked, profileCredentialRevocation{instance: old, email: email})
			}
		}
	}
	return revoked
}
