package xerahttp

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/net"
)

func guardPacketBody(writer http.ResponseWriter, request *http.Request, body io.ReadCloser, session *httpSession, stopped <-chan struct{}) (func(bool) error, error) {
	controller := http.NewResponseController(writer)
	deadline := time.Now().Add(net.ConnIdleTimeout)
	if err := controller.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	finished, joined := make(chan struct{}), make(chan struct{})
	timer := time.NewTimer(time.Until(deadline))
	var failure error
	go func() {
		defer close(joined)
		select {
		case <-finished:
			return
		case <-request.Context().Done():
			failure = request.Context().Err()
		case <-session.uploadQueue.done:
			failure = io.ErrClosedPipe
		case <-stopped:
			failure = context.Canceled
		case <-timer.C:
			failure = context.DeadlineExceeded
		}
		controller.SetReadDeadline(time.Now())
		body.Close()
	}()
	var once sync.Once
	return func(clearDeadline bool) error {
		once.Do(func() {
			close(finished)
			<-joined
			timer.Stop()
			if failure == nil && clearDeadline {
				failure = controller.SetReadDeadline(time.Time{})
			}
		})
		return failure
	}, nil
}
