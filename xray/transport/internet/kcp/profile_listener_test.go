package kcp_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/kcp"
	"github.com/xtls/xray-core/transport/internet/stat"
)

type profileProvider struct{ profile *internet.TCPListenerProfile }

func (p profileProvider) ListenerProfile() *internet.TCPListenerProfile { return p.profile }

func TestManagedKCPReplacementPreservesNativeSession(t *testing.T) {
	pool := internet.NewTCPListenerPool()
	defer pool.Close()
	settings := func() *internet.MemoryStreamConfig {
		return &internet.MemoryStreamConfig{
			ProtocolName: "mkcp", ProtocolSettings: &kcp.Config{Mtu: 1350, Tti: 20, UplinkCapacity: 5, DownlinkCapacity: 20, CwndMultiplier: 20, MaxSendingWindow: 2 * 1024 * 1024},
		}
	}
	accepted := make(chan *kcp.Connection, 4)
	listen := func(profile *internet.TCPListenerProfile, port net.Port, marker byte) *kcp.Listener {
		ctx := internet.ContextWithListenerProfileProvider(context.Background(), profileProvider{profile})
		listener, err := kcp.NewListener(ctx, net.LocalHostIP, port, settings(), func(conn stat.Connection) {
			accepted <- conn.(*kcp.Connection)
			go func() {
				defer conn.Close()
				data := make([]byte, 1)
				for {
					if _, err := io.ReadFull(conn, data); err != nil {
						return
					}
					if _, err := conn.Write([]byte{marker}); err != nil {
						return
					}
				}
			}()
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		return listener
	}
	a := pool.NewProfile(false)
	first := listen(a, 0, 'A')
	port := net.Port(first.Addr().(*net.UDPAddr).Port)
	dial := func() stat.Connection {
		conn, err := kcp.DialKCP(context.Background(), net.UDPDestination(net.LocalHostIP, port), settings())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	assertReply := func(conn stat.Connection, marker byte) {
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte{'?'}); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 1)
		if _, err := io.ReadFull(conn, data); err != nil || data[0] != marker {
			t.Fatalf("KCP reply=%q err=%v want=%q", data, err, marker)
		}
	}
	old := dial()
	assertReply(old, 'A')
	var oldServer *kcp.Connection
	select {
	case oldServer = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("no accepted KCP connection")
	}
	b := pool.NewProfile(true)
	listen(b, port, 'B')
	if err := pool.Switch([]*internet.TCPListenerProfile{b}, []*internet.TCPListenerProfile{a}); err != nil {
		t.Fatal(err)
	}
	assertReply(old, 'A')
	current := dial()
	assertReply(current, 'B')
	select {
	case <-a.Done():
		t.Fatal("old KCP session was cut by replacement")
	default:
	}
	oldServer.SetState(kcp.StateTerminated)
	select {
	case <-a.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("native KCP close did not release retired graph")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	assertReply(current, 'B')
}
