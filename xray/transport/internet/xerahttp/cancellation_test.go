package xerahttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenStreamCanceledBeforeConnection(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	finished := make(chan struct{})
	c := &DefaultDialerClient{transportConfig: &Config{}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		defer close(finished)
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return nil, io.ErrClosedPipe
		}
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		r, _, _, err := c.OpenStream(ctx, "http://localhost/", "session", nil, false)
		if r != nil {
			r.Close()
		}
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenStream ignored cancellation")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("request goroutine survived cancellation")
	}
}

func TestPostPacketCanceledWhileWaiting(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	c := &DefaultDialerClient{transportConfig: &Config{}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		close(started)
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return nil, io.ErrClosedPipe
		}
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- c.PostPacket(ctx, "http://localhost/", "session", "0", buf.MergeBytes(nil, []byte("payload")))
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PostPacket ignored cancellation")
	}
}
