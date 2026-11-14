package outbound

import (
	"context"
	"fmt"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/mux"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/transport/internet"
	_ "github.com/xtls/xray-core/transport/internet/tcp"
	_ "github.com/xtls/xray-core/transport/internet/xdrive"
)

func newMuxConcurrencyHandler(t *testing.T, ctx context.Context, network string, enabled bool, concurrency, xudp int32) *Handler {
	t.Helper()
	settings := &proxyman.SenderConfig{MultiplexSettings: &proxyman.MultiplexingConfig{
		Enabled: enabled, Concurrency: concurrency, XudpConcurrency: xudp, XudpProxyUDP443: "skip",
	}}
	if network != "default" {
		settings.StreamSettings = &internet.StreamConfig{ProtocolName: network}
	}
	created, err := NewHandler(ctx, &core.OutboundHandlerConfig{
		SenderSettings: serial.ToTypedMessage(settings),
		ProxySettings:  serial.ToTypedMessage(&freedom.Config{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, ok := created.(*Handler)
	if !ok {
		t.Fatalf("NewHandler returned %T", created)
	}
	t.Cleanup(func() {
		if err := handler.Close(); err != nil {
			t.Error(err)
		}
	})
	return handler
}

func checkMuxConcurrencyAllocation(t *testing.T, handler *Handler, manager *mux.ClientManager, want uint32) {
	t.Helper()
	if manager == nil || !manager.Enabled {
		t.Fatal("expected an enabled mux manager")
	}
	picker, ok := manager.Picker.(*mux.IncrementalWorkerPicker)
	if !ok {
		t.Fatalf("unexpected worker picker: %T", manager.Picker)
	}
	factory, ok := picker.Factory.(*mux.DialingWorkerFactory)
	if !ok {
		t.Fatalf("unexpected mux factory: %T", picker.Factory)
	}
	if factory.Proxy != handler.proxy || factory.Dialer != handler || factory.Strategy.MaxConnection != 1024 || factory.Strategy.MaxConcurrency != want {
		t.Fatalf("NewHandler wired the wrong effective strategy: %+v, want concurrency=%d", factory.Strategy, want)
	}
	sessions := mux.NewSessionManager()
	t.Cleanup(func() { sessions.Close() })
	var first *mux.Session
	for i := uint32(0); i < want; i++ {
		session := sessions.Allocate(&factory.Strategy, nil, nil)
		if session == nil {
			t.Fatalf("effective strategy rejected session %d before limit %d", i+1, want)
		}
		if first == nil {
			first = session
		}
	}
	if sessions.Allocate(&factory.Strategy, nil, nil) != nil {
		t.Fatal("effective strategy accepted a session beyond its concurrency limit")
	}
	first.Close(false)
	if sessions.Allocate(&factory.Strategy, nil, nil) == nil {
		t.Fatal("effective strategy did not reuse a released concurrency slot")
	}
}

func muxConcurrencyContext(t *testing.T) context.Context {
	t.Helper()
	instance, err := core.New(&core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Error(err)
		}
	})
	return context.WithValue(context.Background(), core.XrayKey(1), instance)
}

func TestHandlerMuxConcurrency(t *testing.T) {
	ctx := muxConcurrencyContext(t)
	for _, network := range []string{"default", "tcp", "xdrive"} {
		for _, test := range []struct {
			configured int32
			normal     int32
			xdrive     int32
		}{{-1, -1, -1}, {0, 8, 128}, {1, 1, 128}, {8, 8, 128}, {127, 127, 128}, {128, 128, 128}, {129, 129, 129}} {
			t.Run(fmt.Sprintf("%s/%d", network, test.configured), func(t *testing.T) {
				handler := newMuxConcurrencyHandler(t, ctx, network, true, test.configured, 0)
				want := test.normal
				if network == "xdrive" {
					want = test.xdrive
				}
				if actual := handler.senderSettings.MultiplexSettings.Concurrency; actual != want {
					t.Errorf("effective sender concurrency=%d, want=%d", actual, want)
				}
				if want < 0 {
					if handler.mux == nil || handler.mux.Enabled || handler.mux.Picker != nil {
						t.Fatal("negative concurrency did not disable mux")
					}
				} else {
					checkMuxConcurrencyAllocation(t, handler, handler.mux, uint32(want))
				}
				if handler.xudp != nil || handler.udp443 != "skip" {
					t.Fatal("mux normalization changed the XUDP zero/default policy")
				}
			})
		}
	}
}

func TestHandlerXUDPConcurrencyUnchanged(t *testing.T) {
	ctx := muxConcurrencyContext(t)
	for _, network := range []string{"tcp", "xdrive"} {
		for _, concurrency := range []int32{-1, 0, 8} {
			for _, xudp := range []int32{-1, 0, 1, 8, 127, 128, 129} {
				t.Run(fmt.Sprintf("%s/mux=%d/xudp=%d", network, concurrency, xudp), func(t *testing.T) {
					handler := newMuxConcurrencyHandler(t, ctx, network, true, concurrency, xudp)
					if handler.senderSettings.MultiplexSettings.XudpConcurrency != xudp || handler.udp443 != "skip" {
						t.Fatal("mux normalization changed explicit XUDP settings")
					}
					switch {
					case xudp < 0:
						if handler.xudp == nil || handler.xudp.Enabled || handler.xudp.Picker != nil {
							t.Fatal("negative XUDP concurrency did not disable XUDP")
						}
					case xudp == 0:
						if handler.xudp != nil {
							t.Fatal("zero XUDP concurrency did not retain the legacy nil manager")
						}
					default:
						checkMuxConcurrencyAllocation(t, handler, handler.xudp, uint32(xudp))
					}
				})
			}
		}
	}
}

func TestHandlerDisabledMuxConcurrency(t *testing.T) {
	ctx := muxConcurrencyContext(t)
	for _, network := range []string{"tcp", "xdrive"} {
		t.Run(network, func(t *testing.T) {
			handler := newMuxConcurrencyHandler(t, ctx, network, false, 8, 8)
			if handler.mux != nil || handler.xudp != nil || handler.udp443 != "" {
				t.Fatal("disabled multiplexing created a manager or enabled a UDP/443 policy")
			}
		})
	}
}
