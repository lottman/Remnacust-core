package internet

import (
	"context"

	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
)

// DialerFeatureProvider keeps profile DNS and proxy chains attached to the
// generation that admitted a connection, including after a configuration commit.
type DialerFeatureProvider interface {
	DialerDNSClient() dns.Client
	DialerOutboundManager() outbound.Manager
}

type dialerFeaturesKey struct{}

func ContextWithDialerFeatures(ctx context.Context, provider DialerFeatureProvider) context.Context {
	return context.WithValue(ctx, dialerFeaturesKey{}, provider)
}

func dialerDNSClient(ctx context.Context) dns.Client {
	if provider, ok := ctx.Value(dialerFeaturesKey{}).(DialerFeatureProvider); ok {
		// An incomplete private scope must never borrow another profile's DNS.
		return provider.DialerDNSClient()
	}
	systemFeaturesMu.RLock()
	defer systemFeaturesMu.RUnlock()
	return dnsClient
}

func dialerOutboundManager(ctx context.Context) outbound.Manager {
	if provider, ok := ctx.Value(dialerFeaturesKey{}).(DialerFeatureProvider); ok {
		return provider.DialerOutboundManager()
	}
	systemFeaturesMu.RLock()
	defer systemFeaturesMu.RUnlock()
	return obm
}
