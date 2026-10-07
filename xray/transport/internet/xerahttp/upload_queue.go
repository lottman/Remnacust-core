package xerahttp

import (
	"container/heap"
	"context"
	"io"
	"sync"

	"github.com/xtls/xray-core/common/errors"
)

type Packet struct {
	Reader  *httpServerConn
	Payload []byte
	Seq     uint64
	lease   *payloadLease
}

type uploadQueue struct {
	mu         sync.Mutex
	changed    *sync.Cond
	reader     *httpServerConn
	heap       uploadHeap
	pending    map[uint64]struct{}
	nextSeq    uint64
	maxPackets int
	closed     bool
}

func NewUploadQueue(maxPackets int) *uploadQueue {
	q := &uploadQueue{maxPackets: max(1, maxPackets), pending: make(map[uint64]struct{})}
	q.changed = sync.NewCond(&q.mu)
	return q
}

func (q *uploadQueue) Push(p Packet) error {
	return q.PushContext(context.Background(), p)
}

func (q *uploadQueue) PushContext(ctx context.Context, p Packet) error {
	owned := true
	defer func() {
		if owned {
			p.lease.release()
		}
	}()
	q.mu.Lock()
	defer q.mu.Unlock()
	var stop func() bool
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	for {
		if q.closed {
			return errors.New("packet queue closed")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if q.reader != nil {
			return errors.New("stream reader already exists")
		}
		if p.Reader != nil {
			if len(q.heap) != 0 || q.nextSeq != 0 {
				return errors.New("packet upload already started")
			}
			q.reader = p.Reader
			q.changed.Broadcast()
			return nil
		}
		if p.Seq < q.nextSeq {
			return nil
		}
		if _, found := q.pending[p.Seq]; found {
			return nil
		}
		if len(q.heap) < q.maxPackets || p.Seq == q.nextSeq {
			heap.Push(&q.heap, p)
			owned = false
			q.pending[p.Seq] = struct{}{}
			q.changed.Broadcast()
			return nil
		}
		if stop == nil {
			stop = context.AfterFunc(ctx, func() {
				q.mu.Lock()
				q.changed.Broadcast()
				q.mu.Unlock()
			})
		}
		q.changed.Wait()
	}
}

func (q *uploadQueue) Close() error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	r := q.reader
	q.reader = nil
	for _, packet := range q.heap {
		packet.lease.release()
	}
	q.heap = nil
	q.pending = nil
	q.changed.Broadcast()
	q.mu.Unlock()
	if r != nil {
		return r.Close()
	}
	return nil
}

func (q *uploadQueue) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	for {
		if q.closed {
			q.mu.Unlock()
			return 0, io.EOF
		}
		if q.reader != nil {
			r := q.reader
			q.mu.Unlock()
			return r.Read(b)
		}
		if len(q.heap) > 0 && q.heap[0].Seq == q.nextSeq {
			p := &q.heap[0]
			n := copy(b, p.Payload)
			p.Payload = p.Payload[n:]
			if len(p.Payload) == 0 {
				p.lease.release()
				delete(q.pending, p.Seq)
				heap.Pop(&q.heap)
				q.nextSeq++
				q.changed.Broadcast()
			}
			if n > 0 {
				q.mu.Unlock()
				return n, nil
			}
			continue
		}
		q.changed.Wait()
	}
}

type uploadHeap []Packet

func (h uploadHeap) Len() int           { return len(h) }
func (h uploadHeap) Less(i, j int) bool { return h[i].Seq < h[j].Seq }
func (h uploadHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *uploadHeap) Push(x any)        { *h = append(*h, x.(Packet)) }
func (h *uploadHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = Packet{}
	*h = old[:n-1]
	return x
}
