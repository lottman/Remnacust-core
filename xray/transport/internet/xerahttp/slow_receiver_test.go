package xerahttp

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/signal/done"
)

type blockedResponseWriter struct {
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (w *blockedResponseWriter) Header() http.Header { return http.Header{} }
func (w *blockedResponseWriter) WriteHeader(int)     {}
func (w *blockedResponseWriter) Flush()              {}
func (w *blockedResponseWriter) Write([]byte) (int, error) {
	close(w.entered)
	<-w.unblock
	return 0, io.ErrClosedPipe
}
func (w *blockedResponseWriter) SetWriteDeadline(time.Time) error {
	w.once.Do(func() { close(w.unblock) })
	return nil
}

func TestServerCloseInterruptsBlockedWrite(t *testing.T) {
	w := &blockedResponseWriter{entered: make(chan struct{}), unblock: make(chan struct{})}
	defer w.SetWriteDeadline(time.Now())
	c := &httpServerConn{Instance: done.New(), ResponseWriter: w}
	written, closed := make(chan struct{}), make(chan struct{})
	go func() { c.Write([]byte("payload")); close(written) }()
	<-w.entered
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("close waited indefinitely for a blocked response write")
		w.SetWriteDeadline(time.Now())
	}
	<-written
	<-closed
}
