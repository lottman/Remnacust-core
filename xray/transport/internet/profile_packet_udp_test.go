package internet

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestSharedUDPPacketSocketKeepsNativeFlowUntilRelease(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a := pool.NewProfile(false)
	ctxA := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{a})
	first, err := ListenProfileUDPPacket(ctxA, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	connA := first.(*profilePacketConn)
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profilePacketSend(t, client, connA.LocalAddr(), []byte{'A'})
	remote := profilePacketRead(t, connA, 'A')
	releaseA, err := connA.AcquirePacketPeer(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	// A broker timeout must never expire a flow the native worker still owns.
	pool.mu.Lock()
	connA.endpoint.peers[profilePacketPeerKey(remote)].expires = time.Now().Add(-time.Hour)
	pool.mu.Unlock()
	b := pool.NewProfile(true)
	ctxB := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{b})
	second, err := ListenProfileUDPPacket(ctxB, connA.LocalAddr().(*net.UDPAddr), &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	connB := second.(*profilePacketConn)
	if _, err := connB.AcquirePacketPeer(remote); err == nil {
		t.Fatal("staged graph stole a native UDP peer")
	}
	if err := pool.Switch([]*TCPListenerProfile{b}, []*TCPListenerProfile{a}); err != nil {
		t.Fatal(err)
	}
	profilePacketSend(t, client, connB.LocalAddr(), []byte{'O'})
	profilePacketRead(t, connA, 'O')
	select {
	case <-a.Done():
		t.Fatal("active UDP flow expired during profile replacement")
	default:
	}
	newClient, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer newClient.Close()
	profilePacketSend(t, newClient, connB.LocalAddr(), []byte{'N'})
	newRemote := profilePacketRead(t, connB, 'N')
	releaseB, err := connB.AcquirePacketPeer(newRemote)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	releaseA()
	releaseA()
	select {
	case <-a.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("closed native UDP flow retained the retired graph")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// Reusing a source socket after its native flow ended starts a new flow.
	profilePacketSend(t, client, connB.LocalAddr(), []byte{'B'})
	remote = profilePacketRead(t, connB, 'B')
	releaseNew, err := connB.AcquirePacketPeer(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNew()
	if _, err := connB.WriteTo([]byte{'R'}, remote); err != nil {
		t.Fatal("old graph closed the shared UDP socket", err)
	}
	profilePacketRead(t, client, 'R')
}

func TestPendingUDPPacketCannotResurrectRetiredFlowAfterExpiry(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a := pool.NewProfile(false)
	ctxA := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{a})
	first, err := ListenProfileUDPPacket(ctxA, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	connA := first.(*profilePacketConn)
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profilePacketSend(t, client, connA.LocalAddr(), []byte{'A'})
	remote := profilePacketRead(t, connA, 'A')
	b := pool.NewProfile(true)
	ctxB := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{b})
	second, err := ListenProfileUDPPacket(ctxB, connA.LocalAddr().(*net.UDPAddr), &MemoryStreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Switch([]*TCPListenerProfile{b}, []*TCPListenerProfile{a}); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	peer := connA.endpoint.peers[profilePacketPeerKey(remote)]
	peer.expires = time.Now().Add(-time.Hour)
	connA.endpoint.deletePacketPeerLocked(peer)
	pool.mu.Unlock()
	if _, err := connA.AcquirePacketPeer(remote); err == nil {
		t.Fatal("expired unaccepted packet became an old-profile flow")
	}
	profilePacketSend(t, client, second.LocalAddr(), []byte{'B'})
	profilePacketRead(t, second, 'B')
}
