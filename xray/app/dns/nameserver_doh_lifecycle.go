package dns

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/common/net"
)

// Track the actual HTTP/2 sockets, including active requests. Closing only idle
// connections can miss a cancelled request that has not returned to the pool.
type dohConnection struct {
	net.Conn
	owner *DoHNameServer
	once  sync.Once
	err   error
}

func (c *dohConnection) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.owner.connectionsAccess.Lock()
		delete(c.owner.connections, c)
		c.owner.connectionsAccess.Unlock()
	})
	return c.err
}

func (s *DoHNameServer) trackConnection(conn net.Conn) (net.Conn, error) {
	s.connectionsAccess.Lock()
	if s.closed {
		s.connectionsAccess.Unlock()
		_ = conn.Close()
		return nil, context.Canceled
	}
	if s.connections == nil {
		s.connections = make(map[*dohConnection]struct{})
	}
	tracked := &dohConnection{Conn: conn, owner: s}
	s.connections[tracked] = struct{}{}
	s.connectionsAccess.Unlock()
	return tracked, nil
}

func (s *DoHNameServer) Close() error {
	_ = s.cacheController.Close()
	s.connectionsAccess.Lock()
	s.closed = true
	connections := make([]*dohConnection, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.connectionsAccess.Unlock()
	if s.httpClient != nil {
		s.httpClient.CloseIdleConnections()
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	return nil
}
