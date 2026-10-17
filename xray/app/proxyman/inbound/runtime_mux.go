package inbound

import (
	"context"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/transport"
)

// Each logical Mux stream starts in the currently applied generation. Existing
// streams and XUDP associations retain their own lease until they finish.
type runtimeMuxDispatcher struct {
	routing.Dispatcher
	slot  *RuntimeSlot
	proxy proxy.Inbound
}

func (d *runtimeMuxDispatcher) BeginSession(ctx context.Context) (context.Context, func(), error) {
	generation, release, ok := d.slot.Acquire()
	if !ok {
		return nil, nil, errors.New("inbound is draining")
	}
	ctx = core.RuntimeContext(ctx, generation.Context)
	ctx, err := d.current(ctx).authenticatedContext(ctx)
	if err != nil {
		release()
		return nil, nil, err
	}
	return ctx, release, nil
}

func (d *runtimeMuxDispatcher) current(ctx context.Context) *runtimeAuthDispatcher {
	return &runtimeAuthDispatcher{Dispatcher: core.MustFromContext(ctx).GetFeature(routing.DispatcherType()).(routing.Dispatcher), proxy: d.proxy, barrier: d.slot.barrier}
}

func (d *runtimeMuxDispatcher) Dispatch(ctx context.Context, destination net.Destination) (*transport.Link, error) {
	return d.current(ctx).Dispatch(ctx, destination)
}
func (d *runtimeMuxDispatcher) DispatchLink(ctx context.Context, destination net.Destination, link *transport.Link) error {
	return d.current(ctx).DispatchLink(ctx, destination, link)
}
