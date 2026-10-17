package hysteria

import (
	"io"
	"sync"
	"testing"
	"time"
)

func TestUDPSessionConcurrentWriteCloseAndCleanupRetirement(t *testing.T) {
	connection := &InterConn{id: 1, ch: make(chan []byte), write: func([]byte) error { return nil }}
	manager := &udpSessionManager{m: map[uint32]*InterConn{1: connection}, udpIdleTimeout: time.Hour}
	connection.close = func() { manager.Lock(); manager.close(connection); manager.Unlock() }
	done := make(chan struct{})
	go func() { manager.clean(); close(done) }()
	var workers sync.WaitGroup
	begin := make(chan struct{})
	for range 32 {
		workers.Go(func() {
			<-begin
			packet := make([]byte, 8)
			for range 128 {
				_, err := connection.Write(packet)
				if err != nil && err != io.ErrClosedPipe {
					t.Error(err)
				}
			}
			_ = connection.Close()
		})
	}
	close(begin)
	manager.stop()
	workers.Wait()
	if _, err := connection.Write(make([]byte, 8)); err != io.ErrClosedPipe {
		t.Fatal("closed UDP session accepted write", err)
	}
	if len(manager.m) != 0 {
		t.Fatal("retired session remained registered")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("UDP session cleanup did not stop")
	}
}
