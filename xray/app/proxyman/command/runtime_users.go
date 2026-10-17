//go:build !wasm

package command

import (
	"context"
	"encoding/hex"
	"sort"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func runtimeUserList(config *serial.TypedMessage) ([]*protocol.MemoryUser, error) {
	settings, err := config.GetInstance()
	if err != nil {
		return nil, err
	}
	field := settings.ProtoReflect().Descriptor().Fields().ByName("users")
	if field == nil || !field.IsList() {
		return nil, status.Error(codes.FailedPrecondition, "This native inbound does not support live users")
	}
	list := settings.ProtoReflect().Get(field).List()
	users := make([]*protocol.MemoryUser, 0, list.Len())
	emails, accounts := make(map[string]bool), make(map[string]bool)
	for i := 0; i < list.Len(); i++ {
		user, ok := list.Get(i).Message().Interface().(*protocol.User)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "Invalid native user record")
		}
		memory, err := user.ToMemoryUser()
		if err != nil {
			return nil, err
		}
		if memory.Email == "" || emails[memory.Email] {
			return nil, status.Error(codes.InvalidArgument, "Live users require unique non-empty emails")
		}
		emails[memory.Email] = true
		canonical := protocol.ToProtoUser(memory).Account
		account, err := canonical.GetInstance()
		if err != nil {
			return nil, err
		}
		if reverse := account.ProtoReflect().Descriptor().Fields().ByName("reverse"); reverse != nil && account.ProtoReflect().Has(reverse) {
			return nil, status.Error(codes.FailedPrecondition, "Native reverse-user changes require an explicit maintenance restart")
		}
		key := canonical.Type + ":"
		identity := account.ProtoReflect().Descriptor().Fields().ByName("id")
		if identity == nil {
			identity = account.ProtoReflect().Descriptor().Fields().ByName("password")
		}
		if identity != nil {
			key += account.ProtoReflect().Get(identity).String()
		} else {
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(account)
			key += hex.EncodeToString(raw)
		}
		if accounts[key] {
			return nil, status.Error(codes.InvalidArgument, "Duplicate native user credentials")
		}
		accounts[key] = true
		users = append(users, memory)
	}
	return users, nil
}

// The runtime snapshot includes API user mutations, not just boot-time JSON.
// This prevents a later reconciliation from overlooking a removed credential.
func (s *handlerServer) runtimeConfiguration(ctx context.Context) *core.Config {
	config := s.s.Configuration()
	if config == nil {
		return nil
	}
	for _, inboundConfig := range config.Inbound {
		handler, err := s.ihm.GetHandler(ctx, inboundConfig.Tag)
		if err != nil {
			continue
		}
		inbound, err := getInbound(handler)
		if err != nil {
			continue
		}
		manager, ok := inbound.(proxy.UserManager)
		if !ok {
			continue
		}
		settings, err := inboundConfig.ProxySettings.GetInstance()
		if err != nil {
			continue
		}
		field := settings.ProtoReflect().Descriptor().Fields().ByName("users")
		if field == nil || !field.IsList() {
			continue
		}
		settings = proto.Clone(settings)
		settings.ProtoReflect().Clear(field)
		list := settings.ProtoReflect().Mutable(field).List()
		users := append([]*protocol.MemoryUser(nil), manager.GetUsers(ctx)...)
		sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })
		for _, user := range users {
			list.Append(protoreflect.ValueOfMessage(protocol.ToProtoUser(user).ProtoReflect()))
		}
		inboundConfig.ProxySettings = serial.ToTypedMessage(settings)
	}
	return config
}
