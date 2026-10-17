package core

import "sync"

type runtimeWorkerToken struct{ allocated byte }

// TrackRuntimeWorker pins the accounting lifetime until a native worker has
// finished. Shutdown stops admission, interrupts registered connections, closes
// transports and waits for these workers before closing its statistics manager.
func (s *Instance) TrackRuntimeWorker(stop func()) (func(), bool) {
	if stop == nil {
		return nil, false
	}
	if s.scopeParent != nil {
		return s.scopeParent.TrackRuntimeWorker(stop)
	}
	s.lifecycleAccess.Lock()
	if s.closing {
		s.lifecycleAccess.Unlock()
		return nil, false
	}
	token := new(runtimeWorkerToken)
	if s.runtimeWorkers == nil {
		s.runtimeWorkers = make(map[*runtimeWorkerToken]func())
	}
	s.runtimeWorkers[token] = stop
	s.runtimeWorkersDone.Add(1)
	s.lifecycleAccess.Unlock()
	return sync.OnceFunc(func() {
		s.lifecycleAccess.Lock()
		delete(s.runtimeWorkers, token)
		s.lifecycleAccess.Unlock()
		s.runtimeWorkersDone.Done()
	}), true
}
