package tls

import (
	"testing"
	"time"
)

func TestStaticCertificateDoesNotStartReloadWorker(t *testing.T) {
	called := make(chan struct{}, 1)
	setupOcspTicker(&Certificate{}, func(bool, bool) { called <- struct{}{} })
	select {
	case <-called:
		t.Fatal("reload worker started without files or OCSP")
	case <-time.After(50 * time.Millisecond):
	}
}
