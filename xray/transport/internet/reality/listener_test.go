package reality

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestOwnedRealityListenerLateHandshakeShutdown(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		inner, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{}, 16)
		listener := newOwnedListener(context.Background(), inner, func(ctx context.Context, conn net.Conn) (net.Conn, error) {
			started <- struct{}{}
			<-ctx.Done()
			return conn, nil
		})
		clients := make([]net.Conn, 0, 16)
		for i := 0; i < 16; i++ {
			conn, err := net.Dial("tcp", inner.Addr().String())
			if err != nil {
				_ = listener.Close()
				t.Fatal(err)
			}
			clients = append(clients, conn)
		}
		for i := 0; i < 16; i++ {
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				_ = listener.Close()
				t.Fatal("handshake did not start")
			}
		}
		_ = listener.Close()
		finished := make(chan struct{})
		go func() { listener.handshakes.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("handshake publication survived listener shutdown")
		}
		if conn, err := listener.Accept(); err == nil {
			_ = conn.Close()
			t.Fatal("closed listener accepted a late handshake")
		}
		for _, conn := range clients {
			_ = conn.Close()
		}
	}
}

func TestOwnedRealityListenerClosePreservesTransferredConnection(t *testing.T) {
	for attempt := 0; attempt < 40; attempt++ {
		inner, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listener := newOwnedListener(context.Background(), inner, func(_ context.Context, conn net.Conn) (net.Conn, error) { return conn, nil })
		client, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			_ = listener.Close()
			t.Fatal(err)
		}
		server, err := listener.Accept()
		if err != nil {
			_ = listener.Close()
			_ = client.Close()
			t.Fatal(err)
		}
		_ = listener.Close()
		_ = client.SetDeadline(time.Now().Add(time.Second))
		_ = server.SetDeadline(time.Now().Add(time.Second))
		if _, err := client.Write([]byte{'!'}); err != nil {
			_ = client.Close()
			_ = server.Close()
			t.Fatal(err)
		}
		var data [1]byte
		_, readErr := io.ReadFull(server, data[:])
		_ = client.Close()
		_ = server.Close()
		if readErr != nil || data[0] != '!' {
			t.Fatal("listener close interrupted transferred ownership", readErr)
		}
		listener.handshakes.Wait()
	}
}
