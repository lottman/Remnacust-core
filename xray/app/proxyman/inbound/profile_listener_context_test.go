package inbound

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
)

type profileListenerCheckKey struct{}
type profileContextListener struct{}

func (profileContextListener) Close() error   { return nil }
func (profileContextListener) Addr() net.Addr { return &net.TCPAddr{} }

var registerProfileContextListener sync.Once
var registerProfileContextListenerError error

func TestProfileListenerKeepsOwnerContextWithoutRequestCancellation(t *testing.T) {
	const transport = "test-profile-owner-context"
	registerProfileContextListener.Do(func() {
		registerProfileContextListenerError = internet.RegisterTransportListener(transport, func(ctx context.Context, _ net.Address, _ net.Port, _ *internet.MemoryStreamConfig, _ internet.ConnHandler) (internet.Listener, error) {
			check, ok := ctx.Value(profileListenerCheckKey{}).(func(context.Context))
			if !ok {
				return nil, errors.New("native listener lost its profile context")
			} else {
				check(ctx)
			}
			return profileContextListener{}, nil
		})
	})
	if registerProfileContextListenerError != nil {
		t.Fatal(registerProfileContextListenerError)
	}
	for _, unix := range []bool{false, true} {
		called := false
		ctx := context.WithValue(context.Background(), profileListenerCheckKey{}, func(ctx context.Context) {
			called = true
			if ctx.Err() != nil || ctx.Done() != nil {
				t.Error("request cancellation owns the persistent listener")
			}
		})
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		stream := &internet.MemoryStreamConfig{ProtocolName: transport}
		var listener worker = &tcpWorker{ctx: ctx, address: net.LocalHostIP, port: 12345, stream: stream}
		if unix {
			listener = &dsWorker{ctx: ctx, address: net.DomainAddress("profile-test.sock"), stream: stream}
		}
		if err := listener.Start(); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Fatal("transport listener was not constructed")
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

type profileListenerLog struct {
	sync.Mutex
	messages []string
}

func (h *profileListenerLog) Handle(message log.Message) {
	h.Lock()
	defer h.Unlock()
	h.messages = append(h.messages, message.String())
}

func TestProfileUDPListenerLogsOnlyToOwnerAfterRequestCancellation(t *testing.T) {
	logger := new(profileListenerLog)
	ctx := log.ContextWithIsolatedHandler(log.ContextWithHandler(context.Background(), logger))
	ctx = context.WithValue(ctx, "cone", false)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	listener := &udpWorker{ctx: ctx, address: net.LocalHostIP, port: 0, stream: &internet.MemoryStreamConfig{}}
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger.Lock()
	defer logger.Unlock()
	if len(logger.messages) != 1 || !strings.Contains(logger.messages[0], "listening UDP on") {
		t.Fatalf("listener log escaped the profile logger: %v", logger.messages)
	}
}
