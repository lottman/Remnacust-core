package connectip

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apernet/quic-go/http3"
)

type failingHTTP3ResponseWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *failingHTTP3ResponseWriter) HTTPStream() (*http3.Stream, error) {
	return nil, w.err
}

func TestProxyPropagatesHTTP3StreamError(t *testing.T) {
	streamErr := errors.New("response header write failed")
	w := &failingHTTP3ResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		err:              streamErr,
	}
	conn, err := (&Proxy{}).Proxy(w, nil)
	if conn != nil || !errors.Is(err, streamErr) {
		t.Fatalf("Proxy() = (%v, %v), want (nil, %v)", conn, err, streamErr)
	}
	if w.Code != http.StatusOK || w.Header().Get(http3.CapsuleProtocolHeader) != capsuleProtocolHeaderValue {
		t.Fatalf("unexpected response: status %d, headers %v", w.Code, w.Header())
	}
}
