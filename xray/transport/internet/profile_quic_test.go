package internet

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/apernet/quic-go"
)

func TestProfileQUICConnectionIDsStayOpaqueUniqueAndAddressIndependent(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	a, b := pool.NewProfile(false), pool.NewProfile(false)
	transports := []*quic.Transport{{}, {}}
	for index, profile := range []*TCPListenerProfile{a, b} {
		ctx := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{profile})
		if err := ConfigureProfileQUIC(ctx, transports[index]); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	for n := 0; n < 1024; n++ {
		for index, profile := range []*TCPListenerProfile{a, b} {
			cid, err := transports[index].ConnectionIDGenerator.GenerateConnectionID()
			if err != nil {
				t.Fatal(err)
			}
			encoded := cid.Bytes()
			if len(encoded) != 16 || seen[string(encoded)] {
				t.Fatal("connection IDs must be unique standard-length opaque values")
			}
			seen[string(encoded)] = true
			short := append([]byte{0x40}, encoded...)
			if pool.ProfileQUICPacketOwner(short) != profile {
				t.Fatal("short-header destination CID lost its profile")
			}
			long := append([]byte{0xc0, 0, 0, 0, 1, 16}, encoded...)
			if pool.ProfileQUICPacketOwner(long) != profile {
				t.Fatal("long-header destination CID lost its profile")
			}
			if binary.BigEndian.Uint64(encoded[:8]) == profile.quicProfileID {
				t.Fatal("wire CID must not expose the internal profile identifier")
			}
		}
	}
	for _, malformed := range [][]byte{nil, {}, {0}, {0x40}, {0xc0, 0, 0, 0, 1, 20}, make([]byte, 32)} {
		if pool.ProfileQUICPacketOwner(malformed) != nil {
			t.Fatal("malformed destination CID acquired a profile")
		}
	}
}

func TestProfileQUICRetirementWaitsForActualConnectionContext(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	profile := pool.NewProfile(false)
	scope := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{profile})
	transport := new(quic.Transport)
	if err := ConfigureProfileQUIC(scope, transport); err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := transport.ConnContext(lifetime, &quic.ClientInfo{}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Switch(nil, []*TCPListenerProfile{profile}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-profile.Done():
		t.Fatal("a live QUIC connection was considered drained")
	default:
	}
	if _, err := transport.ConnContext(lifetime, &quic.ClientInfo{}); err == nil {
		t.Fatal("a new handshake entered the retired profile")
	}
	// Existing transports can rotate CIDs after retirement and network migration.
	cid, err := transport.ConnectionIDGenerator.GenerateConnectionID()
	if err != nil {
		t.Fatal(err)
	}
	packet := append([]byte{0x40}, cid.Bytes()...)
	if pool.ProfileQUICPacketOwner(packet) != profile {
		t.Fatal("an existing connection lost its retired owner")
	}
	cancel()
	select {
	case <-profile.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("closed connection did not release the retired graph")
	}
	if err := profile.Close(); err != nil {
		t.Fatal(err)
	}
	if pool.ProfileQUICPacketOwner(packet) != nil {
		t.Fatal("a closed graph remained addressable through its old CID")
	}
}

func TestProfileQUICRejectsStagedHandshakesAndPreservesUnmanagedTransports(t *testing.T) {
	pool := NewTCPListenerPool()
	defer pool.Close()
	profile := pool.NewProfile(true)
	transport := new(quic.Transport)
	ctx := ContextWithListenerProfileProvider(context.Background(), profileTestProvider{profile})
	if err := ConfigureProfileQUIC(ctx, transport); err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := transport.ConnContext(lifetime, &quic.ClientInfo{}); err == nil {
		t.Fatal("a prepared listener accepted a handshake before commit")
	}
	plain := new(quic.Transport)
	if err := ConfigureProfileQUIC(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	if plain.ConnectionIDGenerator != nil || plain.ConnContext != nil {
		t.Fatal("a standalone transport was modified")
	}
}
