package xerahttp

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestQueueReservedSlotReordersAndDeduplicates(t *testing.T) {
	q := NewUploadQueue(2)
	defer q.Close()
	for _, p := range []Packet{{Seq: 2, Payload: []byte("cd")}, {Seq: 1, Payload: []byte("b")}, {Seq: 1, Payload: []byte("duplicate")}, {Seq: 0, Payload: []byte("a")}} {
		if err := q.Push(p); err != nil {
			t.Fatal(err)
		}
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(q, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcd" {
		t.Fatalf("payload: %q", got)
	}
	if err := q.Push(Packet{Seq: 1, Payload: []byte("old")}); err != nil {
		t.Fatal(err)
	}
	if len(q.heap) != 0 || len(q.pending) != 0 {
		t.Fatal("duplicate retained")
	}
}

func TestQueueCancelBlockedPushAndReleaseOnClose(t *testing.T) {
	q := NewUploadQueue(1)
	defer q.Close()
	if err := q.Push(Packet{Seq: 1, Payload: make([]byte, 1<<20)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- q.PushContext(ctx, Packet{Seq: 2, Payload: []byte("blocked")}) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("push: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked push survived cancellation")
	}
	readResult := make(chan error, 1)
	go func() { _, err := q.Read(make([]byte, 1)); readResult <- err }()
	q.Close()
	select {
	case err := <-readResult:
		if err != io.EOF {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read survived close")
	}
	if q.heap != nil || q.pending != nil || q.reader != nil {
		t.Fatal("closed queue retains data")
	}
	if err := q.Push(Packet{Payload: []byte("late")}); err == nil {
		t.Fatal("push after close succeeded")
	}
}

func TestQueueEmptyPacketsDoNotReturnEmptyRead(t *testing.T) {
	q := NewUploadQueue(2)
	defer q.Close()
	q.Push(Packet{Seq: 0})
	q.Push(Packet{Seq: 1, Payload: []byte("x")})
	b := make([]byte, 1)
	if n, err := q.Read(b); n != 1 || err != nil || b[0] != 'x' {
		t.Fatalf("read: %d, %v, %q", n, err, b)
	}
}
