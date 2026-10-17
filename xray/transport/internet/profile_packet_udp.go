package internet

import (
	"fmt"
	"net"
	"sync"
	"time"
)

type profilePacketPeer struct {
	owner      *profilePacketConn
	key        string
	references int
	pending    bool
	expires    time.Time
}

func profilePacketPeerKey(remote net.Addr, session ...string) string {
	key := ""
	if udp, ok := remote.(*net.UDPAddr); ok {
		copyAddr := *udp
		// Native UDP destinations identify source IP/port within this local
		// socket; their representation has no remote-interface zone field.
		copyAddr.Zone = ""
		key = copyAddr.String()
	} else if remote != nil {
		key = remote.String()
	}
	if key != "" && len(session) > 0 && session[0] != "" {
		key += "\x00" + session[0]
	}
	return key
}

func (e *profilePacketEndpoint) existingPacketPeerLocked(remote net.Addr, session ...string) *profilePacketPeer {
	peer := e.peers[profilePacketPeerKey(remote, session...)]
	if peer == nil {
		return nil
	}
	if peer.owner.closed || peer.references == 0 && !time.Now().Before(peer.expires) {
		e.deletePacketPeerLocked(peer)
		return nil
	}
	return peer
}

func (e *profilePacketEndpoint) newPacketPeerLocked(owner *profilePacketConn, remote net.Addr, session ...string) *profilePacketConn {
	key := profilePacketPeerKey(remote, session...)
	if key == "" || e.pool.packetPendingRoutes >= profilePacketPendingLimit {
		return nil
	}
	peer := &profilePacketPeer{owner: owner, key: key, pending: true, expires: time.Now().Add(10 * time.Second)}
	e.peers[key] = peer
	e.pool.packetPendingRoutes++
	owner.profile.packetConnections++
	return owner
}

func (e *profilePacketEndpoint) deletePacketPeerLocked(peer *profilePacketPeer) {
	if e.peers[peer.key] != peer {
		return
	}
	delete(e.peers, peer.key)
	e.pool.packetPendingRoutes--
	if peer.pending {
		peer.pending = false
		peer.owner.profile.packetConnections--
	}
	peer.owner.profile.finishLocked()
}

// AcquirePacketPeer is paired with the real native UDP flow, not an idle timer
// in the broker. Several original-target flows can share the same source peer.
func (c *profilePacketConn) AcquirePacketPeer(remote net.Addr) (func(), error) {
	return c.acquirePacketPeer(remote, "")
}

func (c *profilePacketConn) AcquirePacketSession(remote net.Addr, session string) (func(), error) {
	if session == "" || len(session) > 64 {
		return nil, fmt.Errorf("invalid managed packet session")
	}
	return c.acquirePacketPeer(remote, session)
}

func (c *profilePacketConn) acquirePacketPeer(remote net.Addr, session string) (func(), error) {
	p := c.profile.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || c.closed || c.quic {
		return nil, net.ErrClosed
	}
	if (c.endpoint.sessionProtocol == "") != (session == "") {
		return nil, fmt.Errorf("packet session protocol does not match listener")
	}
	peer := c.endpoint.existingPacketPeerLocked(remote, session)
	if peer == nil || peer.owner != c {
		return nil, fmt.Errorf("UDP packet peer is not owned by this profile")
	}
	if peer.pending {
		peer.pending = false
		c.profile.packetConnections--
	}
	peer.references++
	c.profile.packetConnections++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			peer.references--
			c.profile.packetConnections--
			if peer.references == 0 {
				c.endpoint.deletePacketPeerLocked(peer)
			}
			c.profile.finishLocked()
			p.mu.Unlock()
		})
	}, nil
}
