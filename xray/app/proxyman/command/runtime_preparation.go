//go:build !wasm

package command

import (
	"context"
	"fmt"

	"github.com/xtls/xray-core/app/dispatcher"
	managedinbound "github.com/xtls/xray-core/app/proxyman/inbound"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// The controller operation lock remains held from preparation through cleanup.
// Preparing binds new listeners with admission disabled, without changing users,
// the active dispatcher, the snapshot or its durable checkpoint.
type runtimePreparation struct {
	server                *handlerServer
	current, target       *core.Config
	registry              *managedinbound.Manager
	staged, retained      []*managedinbound.AlwaysOnInboundHandler
	removed               []string
	users                 []runtimeUsers
	installed             int
	generation            *managedinbound.RuntimeGeneration
	abortGeneration, hold func()
	unchanged, committed  bool
}

func (p *runtimePreparation) abort() {
	if p.committed || p.unchanged {
		return
	}
	for _, handler := range p.staged {
		_ = handler.Close()
	}
	if p.hold != nil {
		p.hold()
		p.hold = nil
	}
	if p.abortGeneration != nil {
		p.abortGeneration()
		p.abortGeneration = nil
	}
}

// Caller holds every admission barrier participating in the transaction.
func (p *runtimePreparation) installUsers(ctx context.Context) error {
	for i, change := range p.users {
		// Include the failing change: a native manager may mutate before erroring.
		p.installed = i + 1
		if err := replaceRuntimeUsers(ctx, change, change.after); err != nil {
			return status.Error(codes.Internal, "Could not replace inbound users; the previous configuration was retained")
		}
	}
	return nil
}

func (p *runtimePreparation) rollbackUsers() ([]func(), error) {
	var rollback error
	for i := p.installed - 1; i >= 0; i-- {
		if err := replaceRuntimeUsers(context.Background(), p.users[i], p.users[i].before); err != nil {
			rollback = err
		}
	}
	if rollback != nil {
		return []func(){p.server.quarantineRuntimeUsersLocked(p.users[:p.installed])}, rollback
	}
	p.installed = 0
	return nil, nil
}

// No fallible I/O or proxy construction is permitted after durable commit.
func (p *runtimePreparation) commitLocked(ctx context.Context) []func() {
	if p.unchanged {
		p.committed = true
		return nil
	}
	s := p.server
	cleanup := p.registry.CommitRuntimeLocked(ctx, p.staged, p.retained, p.removed, p.generation)
	if p.hold != nil {
		if s.runtime.hold != nil {
			cleanup = append(cleanup, s.runtime.hold)
		}
		s.runtime.hold = p.hold
	}
	s.runtime.generation = p.generation
	s.runtime.sequence++
	s.s.RecordConfiguration(p.target)
	p.committed = true
	return cleanup
}

func (p *runtimePreparation) finish(cleanup []func()) {
	for _, close := range cleanup {
		close()
	}
	for _, change := range p.users {
		target := make(map[string]*protocol.MemoryUser, len(change.after))
		for _, user := range change.after {
			target[user.Email] = user
		}
		for _, user := range change.before {
			if next := target[user.Email]; next == nil || !proto.Equal(protocol.ToProtoUser(user), protocol.ToProtoUser(next)) {
				dispatcher.RevokeProfileInboundUserLinks(p.server.s, change.tag, user.Email)
			}
		}
	}
}

func (p *runtimePreparation) result() (*structpb.Struct, error) {
	return structpb.NewStruct(map[string]interface{}{"hash": runtimeHash(p.target), "generation": fmt.Sprint(p.server.runtime.sequence), "unchanged": p.unchanged})
}
