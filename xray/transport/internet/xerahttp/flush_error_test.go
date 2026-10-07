package xerahttp

import (
	"errors"
	"net/http"
	"testing"

	"github.com/xtls/xray-core/common/signal/done"
)

type failedFlushWriter struct{ err error }

func (w *failedFlushWriter) Header() http.Header         { return http.Header{} }
func (w *failedFlushWriter) WriteHeader(int)             {}
func (w *failedFlushWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *failedFlushWriter) Flush()                      {}
func (w *failedFlushWriter) FlushError() error           { return w.err }

func TestServerWriteReportsFlushFailure(t *testing.T) {
	failure := errors.New("connection failed during flush")
	c := &httpServerConn{Instance: done.New(), ResponseWriter: &failedFlushWriter{err: failure}}
	defer c.Close()
	n, err := c.Write([]byte("payload"))
	if n != 7 || !errors.Is(err, failure) {
		t.Fatalf("Write returned (%d, %v), want (7, flush failure)", n, err)
	}
}
