package inbound

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/transport"
)

type runtimeAuthDispatcher struct {
	routing.Dispatcher
	proxy   proxy.Inbound
	barrier *sync.RWMutex
}

// Cached transport authentication is insufficient for a new Mux/QUIC stream.
// The native credential store remains authoritative when the panel is offline.
func (d *runtimeAuthDispatcher) authorize(ctx context.Context) error {
	release, err := d.admit(ctx)
	if release != nil {
		release()
	}
	return err
}

func (d *runtimeAuthDispatcher) admit(ctx context.Context) (func(), error) {
	release := func() {}
	if d.barrier != nil {
		d.barrier.RLock()
		release = d.barrier.RUnlock
	}
	if instance := core.FromContext(ctx); instance != nil && instance.RuntimeAdmissionFenced() {
		release()
		return nil, errors.New("profile admission requires administrator recovery")
	}
	inbound := session.InboundFromContext(ctx)
	manager, ok := d.proxy.(proxy.UserManager)
	if !ok || inbound == nil || inbound.User == nil || inbound.User.Email == "" {
		return release, nil
	}
	current := manager.GetUser(ctx, inbound.User.Email)
	if current == nil || current.Level != inbound.User.Level || current.Account == nil || inbound.User.Account == nil || !current.Account.Equals(inbound.User.Account) {
		release()
		return nil, errors.New("authenticated identity is no longer allowed on this inbound")
	}
	return release, nil
}

func (d *runtimeAuthDispatcher) authenticatedContext(ctx context.Context) (context.Context, error) {
	if d.barrier != nil {
		d.barrier.RLock()
		defer d.barrier.RUnlock()
	}
	inbound := session.InboundFromContext(ctx)
	manager, ok := d.proxy.(proxy.UserManager)
	if !ok || inbound == nil || inbound.User == nil || inbound.User.Email == "" {
		return ctx, nil
	}
	current := manager.GetUser(ctx, inbound.User.Email)
	if current == nil || current.Account == nil || inbound.User.Account == nil || !current.Account.Equals(inbound.User.Account) {
		return nil, errors.New("authenticated identity is no longer allowed on this inbound")
	}
	copy := *inbound
	copy.User = current
	return session.ContextWithInbound(ctx, &copy), nil
}

func (d *runtimeAuthDispatcher) Dispatch(ctx context.Context, destination net.Destination) (*transport.Link, error) {
	ctx, err := d.authenticatedContext(ctx)
	if err != nil {
		return nil, err
	}
	return d.Dispatcher.Dispatch(session.ContextWithAdmission(ctx, d.admit), destination)
}

func (d *runtimeAuthDispatcher) DispatchLink(ctx context.Context, destination net.Destination, link *transport.Link) error {
	ctx, err := d.authenticatedContext(ctx)
	if err != nil {
		return err
	}
	return d.Dispatcher.DispatchLink(session.ContextWithAdmission(ctx, d.admit), destination, link)
}
