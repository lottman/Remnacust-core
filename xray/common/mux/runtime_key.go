package mux

import (
	"context"
	"crypto/sha256"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"google.golang.org/protobuf/proto"
)

// The wire GlobalID is client-controlled and cannot identify an authenticated
// server-side association across users, ports or routing generations by itself.
func xudpSessionKey(ctx context.Context, id [8]byte) XUDPSessionKey {
	key := XUDPSessionKey{Core: core.FromContext(ctx), GlobalID: id}
	if inbound := session.InboundFromContext(ctx); inbound != nil {
		key.Tag, key.Port = inbound.Tag, inbound.Gateway.String()
		if inbound.User != nil && inbound.User.Account != nil {
			key.Identity = inbound.User.Email
			data, _ := proto.MarshalOptions{Deterministic: true}.Marshal(protocol.ToProtoUser(inbound.User))
			key.Credential = sha256.Sum256(data)
		} else {
			key.Identity = inbound.Source.String()
		}
	}
	return key
}
