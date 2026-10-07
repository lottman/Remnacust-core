package xerahttp

import "sync/atomic"

func acquireSlot(count *atomic.Int64, limit int64) bool {
	for {
		current := count.Load()
		if current >= limit {
			return false
		}
		if count.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

type payloadBudget struct {
	limit int64
	used  atomic.Int64
}

type payloadLease struct {
	budget *payloadBudget
	bytes  atomic.Int64
}

func (b *payloadBudget) acquire(size int64) *payloadLease {
	if size < 0 || size > b.limit {
		return nil
	}
	for {
		used := b.used.Load()
		if used > b.limit-size {
			return nil
		}
		if b.used.CompareAndSwap(used, used+size) {
			lease := &payloadLease{budget: b}
			lease.bytes.Store(size)
			return lease
		}
	}
}

func (l *payloadLease) release() {
	if l != nil {
		l.budget.used.Add(-l.bytes.Swap(0))
	}
}

func (l *payloadLease) shrink(size int64) {
	previous := l.bytes.Load()
	if size >= 0 && size < previous {
		l.bytes.Store(size)
		l.budget.used.Add(size - previous)
	}
}

func (c *Config) serverMemoryBudget() int64 {
	if c.ServerMaxBufferedBytes == 0 {
		return 64 << 20
	}
	return c.ServerMaxBufferedBytes
}

func (c *Config) serverSessionLimit() int64 {
	if c.ServerMaxSessions == 0 {
		return 4096
	}
	return int64(c.ServerMaxSessions)
}
