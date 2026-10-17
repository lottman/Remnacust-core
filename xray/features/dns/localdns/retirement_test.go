package localdns

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
)

func TestRetirementCancelsSystemLookupAndRejectsRestart(t *testing.T) {
	original := net.DefaultResolver
	started, finished := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	t.Cleanup(func() { net.DefaultResolver = original })
	client := New()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_, _, err := client.LookupIPContext(ctx, "retirement.invalid", dns.IPOption{IPv4Enable: true})
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("system resolver did not start")
	}
	_ = client.Close()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("retired system lookup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("system lookup ignored retirement")
	}
	if _, _, err := client.LookupIP("127.0.0.1", dns.IPOption{IPv4Enable: true}); err != context.Canceled {
		t.Fatal("closed local resolver accepted a lookup", err)
	}
}
