package internet

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func profileTestBind(ctx context.Context, addr net.Addr, _ *SocketConfig) (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(ctx, "tcp", addr.String())
}

func profileTestServe(t *testing.T, listener net.Listener, label byte) {
	t.Helper()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var value [1]byte
				for {
					if _, err := io.ReadFull(conn, value[:]); err != nil {
						return
					}
					if _, err := conn.Write([]byte{label}); err != nil {
						return
					}
				}
			}()
		}
	}()
}

func profileTestExchange(t *testing.T, conn net.Conn, want byte) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte{'?'}); err != nil {
		t.Fatal(err)
	}
	var got [1]byte
	if _, err := io.ReadFull(conn, got[:]); err != nil {
		t.Fatal(err)
	}
	if got[0] != want {
		t.Fatalf("got %c, want %c", got[0], want)
	}
}

func TestProfileTCPListenerSwitchPreservesConnections(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a := pool.NewProfile(false)
	listener, err := a.Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, nil, profileTestBind)
	if err != nil {
		t.Fatal(err)
	}
	profileTestServe(t, listener, 'a')
	address := listener.Addr().(*net.TCPAddr)
	old, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	profileTestExchange(t, old, 'a')
	b := pool.NewProfile(true)
	next, err := b.Listen(context.Background(), address, nil, profileTestBind)
	if err != nil {
		t.Fatal(err)
	}
	profileTestServe(t, next, 'b')
	probe, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	profileTestExchange(t, probe, 'a')
	_ = probe.Close()
	if err := pool.Switch([]*TCPListenerProfile{b}, nil); err == nil {
		t.Fatal("stole an unretired profile's port")
	}
	if err := pool.Switch([]*TCPListenerProfile{b}, []*TCPListenerProfile{a}); err != nil {
		t.Fatal(err)
	}
	current, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	profileTestExchange(t, current, 'b')
	profileTestExchange(t, old, 'a')
	select {
	case <-a.Done():
		t.Fatal("retired graph drained with a live connection")
	default:
	}
	_ = old.Close()
	select {
	case <-a.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("old graph did not drain")
	}
	_ = a.Close()
	profileTestExchange(t, current, 'b')
	_ = pool.Close()
	_ = current.SetDeadline(time.Now().Add(time.Second))
	var value [1]byte
	if _, err := current.Read(value[:]); err == nil {
		t.Fatal("pool close left a live connection")
	}
}

func TestProfileTCPListenerClosesDetachedListenerConnections(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a := pool.NewProfile(false)
	l, err := a.Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, nil, profileTestBind)
	if err != nil {
		t.Fatal(err)
	}
	profileTestServe(t, l, 'a')
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	profileTestExchange(t, c, 'a')
	_ = l.Close()
	profileTestExchange(t, c, 'a')
	_ = pool.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	var value [1]byte
	if _, err := c.Read(value[:]); err == nil {
		t.Fatal("detached listener connection survived shutdown")
	}
	if _, err := pool.NewProfile(true).Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, nil, profileTestBind); err == nil {
		t.Fatal("closed pool accepted a listener")
	}
}

func TestProfileTCPListenerRejectsSocketChangeAndAbortsStage(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a := pool.NewProfile(false)
	l, err := a.Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, nil, profileTestBind)
	if err != nil {
		t.Fatal(err)
	}
	profileTestServe(t, l, 'a')
	address := l.Addr().(*net.TCPAddr)
	staged := pool.NewProfile(true)
	if _, err := staged.Listen(context.Background(), address, &SocketConfig{Mark: 1}, profileTestBind); err == nil {
		t.Fatal("changed kernel socket options were silently reused")
	}
	prepared, err := staged.Listen(context.Background(), address, nil, profileTestBind)
	if err != nil {
		t.Fatal(err)
	}
	_ = prepared.Close()
	_ = staged.Close()
	c, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	profileTestExchange(t, c, 'a')
	if err := pool.Switch([]*TCPListenerProfile{staged}, []*TCPListenerProfile{a}); err == nil {
		t.Fatal("closed preparation committed")
	}
	profileTestExchange(t, c, 'a')
}

func TestProfileTCPListenerWildcardReplacement(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	// Obtain a free port without assuming how Go represents wildcard binds.
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	_ = reservation.Close()
	address := &net.TCPAddr{IP: net.IPv4zero, Port: port}
	a := pool.NewProfile(false)
	l, err := a.Listen(context.Background(), address, nil, profileTestBind)
	if err != nil {
		t.Fatal(err)
	}
	profileTestServe(t, l, 'a')
	b := pool.NewProfile(true)
	next, err := b.Listen(context.Background(), address, nil, profileTestBind)
	if err != nil {
		t.Fatal(err)
	}
	profileTestServe(t, next, 'b')
	if err := pool.Switch([]*TCPListenerProfile{b}, []*TCPListenerProfile{a}); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	profileTestExchange(t, c, 'b')
}

func TestProfileTCPListenerQueueCloseReleasesOwnership(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		pool := NewTCPListenerPool()
		profile := pool.NewProfile(false)
		listener, err := profile.Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, nil, profileTestBind)
		if err != nil {
			_ = pool.Close()
			t.Fatal(err)
		}
		connections := make([]net.Conn, 0, 20)
		for i := 0; i < 20; i++ {
			c, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				_ = pool.Close()
				t.Fatal(err)
			}
			connections = append(connections, c)
		}
		_ = listener.Close()
		deadline := time.Now().Add(3 * time.Second)
		for {
			pool.mu.Lock()
			remaining := len(profile.connections)
			pool.mu.Unlock()
			if remaining == 0 {
				break
			}
			if time.Now().After(deadline) {
				_ = pool.Close()
				t.Fatal("closed listener stranded queued connections", remaining)
			}
			time.Sleep(time.Millisecond)
		}
		for _, c := range connections {
			_ = c.Close()
		}
		_ = pool.Close()
	}
}

type profileTestProvider struct{ profile *TCPListenerProfile }

func (p profileTestProvider) ListenerProfile() *TCPListenerProfile { return p.profile }

func TestProfileTCPStageCannotBindPacketSocket(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	active := pool.NewProfile(false)
	ctx := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{active})
	packet, err := ListenSystemPacket(ctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	staged := pool.NewProfile(true)
	stageCtx := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{staged})
	if conn, err := ListenSystemPacket(stageCtx, packet.LocalAddr(), nil); err == nil {
		_ = conn.Close()
		t.Fatal("TCP staging bound a UDP socket")
	}
	sender, err := net.Dial("udp", packet.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if _, err := sender.Write([]byte("old UDP stays available")); err != nil {
		t.Fatal(err)
	}
	_ = packet.SetReadDeadline(time.Now().Add(time.Second))
	var data [64]byte
	if n, _, err := packet.ReadFrom(data[:]); err != nil || string(data[:n]) != "old UDP stays available" {
		t.Fatal("staging interrupted the active UDP listener", err)
	}
	_ = active.Close()
	if err := CheckPacketListenerProfile(ctx); err == nil {
		t.Fatal("closed profile can bind packet sockets")
	}
}
