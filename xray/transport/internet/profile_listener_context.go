package internet

import (
	"context"
	"fmt"
	"net"
)

type listenerProfileKey struct{}

type ListenerProfileProvider interface {
	ListenerProfile() *TCPListenerProfile
}

func ContextWithListenerProfileProvider(ctx context.Context, provider ListenerProfileProvider) context.Context {
	return context.WithValue(ctx, listenerProfileKey{}, provider)
}

// Until packet-flow ownership is available, preparing a TCP replacement must
// never bind a UDP/QUIC socket and steal packets through kernel reuse-port.
func CheckPacketListenerProfile(ctx context.Context) error {
	provider, ok := ctx.Value(listenerProfileKey{}).(ListenerProfileProvider)
	if !ok {
		return nil
	}
	v := provider.ListenerProfile()
	v.pool.mu.Lock()
	defer v.pool.mu.Unlock()
	if v.closed || v.retired || v.pool.closed {
		return net.ErrClosed
	}
	if v.staged {
		return fmt.Errorf("packet-flow ownership is required before staging UDP or QUIC listeners")
	}
	v.packetListeners = true
	return nil
}
