package internet

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/apernet/quic-go"
)

func profilePacketRead(t *testing.T, conn net.PacketConn, want byte) net.Addr {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2048)
	n, remote, err := conn.ReadFrom(data)
	if err != nil || n == 0 || data[n-1] != want {
		t.Fatalf("packet: bytes=%d err=%v want=%q", n, err, want)
	}
	return remote
}

func profilePacketSend(t *testing.T, conn net.PacketConn, destination net.Addr, packet []byte) {
	t.Helper()
	if _, err := conn.WriteTo(packet, destination); err != nil {
		t.Fatal(err)
	}
}

func profileTestInitial(id byte) []byte {
	packet := []byte{0xc0, 0, 0, 0, 1, 20}
	for n := 0; n < 20; n++ {
		packet = append(packet, id)
	}
	return append(packet, id)
}

func TestSharedQUICPacketSocketSwitchKeepsOldCIDAndMigratedPeer(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a := pool.NewProfile(false)
	ctxA := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{a})
	connA, err := ListenProfileQUICPacket(ctxA, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	transportA := new(quic.Transport)
	if err := ConfigureProfileQUIC(ctxA, transportA); err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profilePacketSend(t, client, connA.LocalAddr(), profileTestInitial('A'))
	remote := profilePacketRead(t, connA, 'A')
	lifetimeA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	if _, err := transportA.ConnContext(lifetimeA, &quic.ClientInfo{RemoteAddr: remote}); err != nil {
		t.Fatal(err)
	}
	cidA, err := transportA.ConnectionIDGenerator.GenerateConnectionID()
	if err != nil {
		t.Fatal(err)
	}
	b := pool.NewProfile(true)
	ctxB := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{b})
	connB, err := ListenProfileQUICPacket(ctxB, connA.LocalAddr().(*net.UDPAddr), &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	transportB := new(quic.Transport)
	if err := ConfigureProfileQUIC(ctxB, transportB); err != nil {
		t.Fatal(err)
	}
	if err := pool.CommitSwitch([]*TCPListenerProfile{b}, []*TCPListenerProfile{a}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
		t.Fatal("old QUIC connection was drained during socket switch")
	default:
	}
	migrated, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	packet := append([]byte{0x40}, cidA.Bytes()...)
	packet = append(packet, 'M')
	profilePacketSend(t, migrated, connB.LocalAddr(), packet)
	if got := profilePacketRead(t, connA, 'M'); got.String() != migrated.LocalAddr().String() {
		t.Fatal("migrated source was replaced")
	}
	profilePacketSend(t, client, connB.LocalAddr(), profileTestInitial('B'))
	remote = profilePacketRead(t, connB, 'B')
	lifetimeB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if _, err := transportB.ConnContext(lifetimeB, &quic.ClientInfo{RemoteAddr: remote}); err != nil {
		t.Fatal(err)
	}
	cancelA()
	select {
	case <-a.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("closed QUIC connection retained old profile")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := connB.WriteTo([]byte{'R'}, client.LocalAddr()); err != nil {
		t.Fatal("closing old graph closed shared socket", err)
	}
	profilePacketRead(t, client, 'R')
}

func TestSharedQUICPacketSocketFailedCommitAndDecoderMismatchLeaveOldOwner(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a := pool.NewProfile(false)
	ctxA := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{a})
	connA, err := ListenProfileQUICPacket(ctxA, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	b := pool.NewProfile(true)
	ctxB := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{b})
	if _, err := ListenProfileQUICPacket(ctxB, connA.LocalAddr().(*net.UDPAddr), &MemoryStreamConfig{PacketMaskID: "different-decoder"}); err == nil {
		t.Fatal("incompatible decoder stole the physical socket")
	}
	connB, err := ListenProfileQUICPacket(ctxB, connA.LocalAddr().(*net.UDPAddr), &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("checkpoint fsync rejected")
	if err := pool.CommitSwitch([]*TCPListenerProfile{b}, []*TCPListenerProfile{a}, func() error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profilePacketSend(t, client, connA.LocalAddr(), profileTestInitial('A'))
	profilePacketRead(t, connA, 'A')
	if err := connB.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connB.ReadFrom(make([]byte, 128)); err == nil {
		t.Fatal("staged profile received a packet before commit")
	}
	pool.mu.Lock()
	unchanged := connA.(*profilePacketConn).endpoint.active == connA
	pool.mu.Unlock()
	if !unchanged {
		t.Fatal("failed commit changed active ownership")
	}
}

func TestSharedPacketCloseReleasesQueuesAndPendingHandshakePins(t *testing.T) {
	pool := NewTCPListenerPool()
	a := pool.NewProfile(false)
	ctx := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{a})
	conn, err := ListenProfileQUICPacket(ctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profilePacketSend(t, client, conn.LocalAddr(), profileTestInitial('A'))
	profilePacketRead(t, conn, 'A')
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.packetQueuedBytes != 0 || pool.packetPendingRoutes != 0 || a.packetConnections != 0 || len(pool.packetEndpoints) != 0 {
		t.Fatal("packet resources survived pool shutdown")
	}
}
