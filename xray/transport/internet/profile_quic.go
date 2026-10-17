package internet

import (
	"context"
	"crypto/aes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/apernet/quic-go"
)

const profileQUICConnectionIDLength = aes.BlockSize

type profileQUICConnectionIDs struct {
	profile *TCPListenerProfile
}

func (g *profileQUICConnectionIDs) ConnectionIDLen() int { return profileQUICConnectionIDLength }

func (g *profileQUICConnectionIDs) GenerateConnectionID() (quic.ConnectionID, error) {
	var plain, encoded [profileQUICConnectionIDLength]byte
	p := g.profile.pool
	p.mu.Lock()
	if p.closed || g.profile.closed {
		p.mu.Unlock()
		return quic.ConnectionID{}, net.ErrClosed
	}
	id, block := g.profile.quicProfileID, p.quicCipher
	if g.profile.quicCIDSequence == ^uint64(0) {
		p.mu.Unlock()
		return quic.ConnectionID{}, fmt.Errorf("profile QUIC connection identifier space exhausted")
	}
	g.profile.quicCIDSequence++
	sequence := g.profile.quicCIDSequence
	p.mu.Unlock()
	if id == 0 || block == nil {
		return quic.ConnectionID{}, fmt.Errorf("profile QUIC routing is not initialized")
	}
	binary.BigEndian.PutUint64(plain[:8], id)
	binary.BigEndian.PutUint64(plain[8:], sequence)
	// Encrypt the complete identifier, not a visible profile prefix. Standard
	// clients see opaque, unrelated CIDs and require no application extension.
	block.Encrypt(encoded[:], plain[:])
	return quic.ConnectionIDFromBytes(encoded[:]), nil
}

func (v *TCPListenerProfile) quicConnectionIDs() (quic.ConnectionIDGenerator, error) {
	p := v.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || v.closed || v.retired {
		return nil, net.ErrClosed
	}
	if p.quicCipher == nil {
		var key [32]byte
		if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(key[:])
		if err != nil {
			return nil, err
		}
		p.quicCipher = block
		p.quicProfiles = make(map[uint64]*TCPListenerProfile)
	}
	if v.quicProfileID == 0 {
		if p.quicNextID == ^uint64(0) {
			return nil, fmt.Errorf("profile QUIC identifier space exhausted")
		}
		p.quicNextID++
		v.quicProfileID = p.quicNextID
		p.quicProfiles[v.quicProfileID] = v
	}
	return &profileQUICConnectionIDs{profile: v}, nil
}

// ProfileQUICPacketOwner resolves a server-issued destination CID independently
// of source address, so a QUIC connection can migrate between client networks.
// This is a routing hint only; QUIC still authenticates every packet itself.
func (p *TCPListenerPool) ProfileQUICPacketOwner(packet []byte) *TCPListenerProfile {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.profileQUICPacketOwnerLocked(packet)
}

func (p *TCPListenerPool) profileQUICPacketOwnerLocked(packet []byte) *TCPListenerProfile {
	if p.closed || p.quicCipher == nil || len(packet) == 0 {
		return nil
	}
	var cid []byte
	if packet[0]&0x80 != 0 {
		if len(packet) < 6 || int(packet[5]) != profileQUICConnectionIDLength || len(packet) < 6+profileQUICConnectionIDLength {
			return nil
		}
		cid = packet[6 : 6+profileQUICConnectionIDLength]
	} else {
		if packet[0]&0x40 == 0 || len(packet) < 1+profileQUICConnectionIDLength {
			return nil
		}
		cid = packet[1 : 1+profileQUICConnectionIDLength]
	}
	var plain [profileQUICConnectionIDLength]byte
	p.quicCipher.Decrypt(plain[:], cid)
	owner := p.quicProfiles[binary.BigEndian.Uint64(plain[:8])]
	if owner == nil || owner.closed {
		return nil
	}
	return owner
}

func (v *TCPListenerProfile) acquireQUICConnection(remote net.Addr) (func(), error) {
	p := v.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || v.closed || v.staged {
		return nil, net.ErrClosed
	}
	claimed := false
	if remote != nil {
		for pending := range v.quicPending[remote.String()] {
			if pending.pending && time.Now().Before(pending.expires) {
				pending.clearPendingLocked()
				claimed = true
				break
			}
		}
	}
	if v.retired && !claimed {
		return nil, net.ErrClosed
	}
	v.packetConnections++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			v.packetConnections--
			v.finishLocked()
			p.mu.Unlock()
		})
	}, nil
}

// ConfigureProfileQUIC ties retirement to the real connection context, covering
// handshakes, streams and datagrams. A new handshake cannot enter a retired graph.
func ConfigureProfileQUIC(scope context.Context, transport *quic.Transport) error {
	provider, ok := scope.Value(listenerProfileKey{}).(ListenerProfileProvider)
	if !ok {
		return nil
	}
	if transport == nil || transport.ConnectionIDGenerator != nil || transport.ConnContext != nil {
		return fmt.Errorf("profile QUIC hooks are already configured")
	}
	profile := provider.ListenerProfile()
	if profile == nil {
		return fmt.Errorf("profile QUIC owner is missing")
	}
	generator, err := profile.quicConnectionIDs()
	if err != nil {
		return err
	}
	transport.ConnectionIDGenerator = generator
	transport.ConnContext = func(ctx context.Context, info *quic.ClientInfo) (context.Context, error) {
		if ctx.Done() == nil {
			return nil, fmt.Errorf("a native QUIC connection lifetime is required")
		}
		if gate, ok := provider.(interface{ RuntimeAccess() *sync.RWMutex }); ok {
			barrier := gate.RuntimeAccess()
			barrier.RLock()
			defer barrier.RUnlock()
		}
		if gate, ok := provider.(interface{ RuntimeAdmissionFenced() bool }); ok && gate.RuntimeAdmissionFenced() {
			return nil, net.ErrClosed
		}
		var remote net.Addr
		if info != nil {
			remote = info.RemoteAddr
		}
		release, err := profile.acquireQUICConnection(remote)
		if err != nil {
			return nil, err
		}
		context.AfterFunc(ctx, release)
		return ctx, nil
	}
	return nil
}
