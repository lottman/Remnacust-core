package xerahttp

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestTransportConnReadDeadlineReleasesBlockedRead(t *testing.T) {
	reader, readerWriter := io.Pipe()
	writerReader, writer := io.Pipe()
	conn := &transportConn{reader: reader, writer: writer}
	defer conn.Close()
	result := make(chan error, 1)
	go func() { _, err := conn.Read(make([]byte, 1)); result <- err }()
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline error lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read remained blocked after deadline")
	}
	writerReader.Close()
	readerWriter.Close()
}

func TestTransportConnWriteDeadlineReleasesBlockedWrite(t *testing.T) {
	reader, readerWriter := io.Pipe()
	writerReader, writer := io.Pipe()
	conn := &transportConn{reader: reader, writer: writer}
	defer conn.Close()
	result := make(chan error, 1)
	go func() { _, err := conn.Write([]byte("x")); result <- err }()
	if err := conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline error lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write remained blocked after deadline")
	}
	writerReader.Close()
	readerWriter.Close()
}

func TestCanceledDeadlineCallbackDoesNotCloseConnection(t *testing.T) {
	for _, read := range []bool{false, true} {
		conn := &transportConn{reader: &trackedCloser{}, writer: &trackedCloser{}}
		if err := conn.setDeadline(time.Now().Add(time.Hour), read); err != nil {
			t.Fatal(err)
		}
		if err := conn.setDeadline(time.Time{}, read); err != nil {
			t.Fatal(err)
		}
		conn.expireDeadline(read, 1)
		if conn.closed.Load() || conn.timedOut.Load() {
			t.Fatal("stale callback closed connection after deadline was cleared")
		}
		conn.Close()
	}
}
