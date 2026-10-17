package internet

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestSharedPacketSessionsSeparateConversationsOnSameSource(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	key := func(packet []byte) string {
		if len(packet) < 2 {
			return ""
		}
		return string(packet[:2])
	}
	a := pool.NewProfile(false)
	ctxA := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{a})
	first, err := ListenProfileSessionPacket(ctxA, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &MemoryStreamConfig{}, "test-conversation-v1", key)
	if err != nil {
		t.Fatal(err)
	}
	connA := first.(*profilePacketConn)
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profilePacketSend(t, client, connA.LocalAddr(), []byte{0, 1, 'A'})
	remote := profilePacketRead(t, connA, 'A')
	releaseA, err := connA.AcquirePacketSession(remote, string([]byte{0, 1}))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	if _, err := connA.AcquirePacketPeer(remote); err == nil {
		t.Fatal("session listener accepted unscoped native flow")
	}
	b := pool.NewProfile(true)
	ctxB := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{b})
	second, err := ListenProfileSessionPacket(ctxB, connA.LocalAddr().(*net.UDPAddr), &MemoryStreamConfig{}, "test-conversation-v1", key)
	if err != nil {
		t.Fatal(err)
	}
	connB := second.(*profilePacketConn)
	if _, err := connB.AcquirePacketSession(remote, string([]byte{0, 1})); err == nil {
		t.Fatal("staged session stole old conversation")
	}
	if err := pool.Switch([]*TCPListenerProfile{b}, []*TCPListenerProfile{a}); err != nil {
		t.Fatal(err)
	}
	profilePacketSend(t, client, connB.LocalAddr(), []byte{0, 2, 'B'})
	profilePacketRead(t, connB, 'B')
	releaseB, err := connB.AcquirePacketSession(remote, string([]byte{0, 2}))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	profilePacketSend(t, client, connB.LocalAddr(), []byte{0, 1, 'O'})
	profilePacketRead(t, connA, 'O')
	select {
	case <-a.Done():
		t.Fatal("old live session expired")
	default:
	}
	releaseA()
	releaseA()
	select {
	case <-a.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("closed conversation retained graph")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	profilePacketSend(t, client, connB.LocalAddr(), []byte{0, 1, 'N'})
	profilePacketRead(t, connB, 'N')
	releaseNew, err := connB.AcquirePacketSession(remote, string([]byte{0, 1}))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNew()
	if _, err := connB.WriteTo([]byte{'R'}, remote); err != nil {
		t.Fatal(err)
	}
	profilePacketRead(t, client, 'R')
}
