package router

import "sync"

// modelStopOperation represents one planned stop generation. Concurrent stop
// callers join the same operation so lifecycle hooks and Process.Stop execute
// once, while every caller can wait for the shared result to complete.
type modelStopOperation struct {
	drained chan struct{}
	done    chan struct{}
}

// modelStopState fences request grants against a planned stop. A request is
// counted before its handler is handed to the caller, closing the small race
// between the scheduler's ready-state check and the actual ServeHTTP call.
type modelStopState struct {
	mu     sync.Mutex
	active int
	stop   *modelStopOperation
}

func (s *modelStopState) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return false
	}
	s.active++
	return true
}

func (s *modelStopState) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active <= 0 {
		return
	}
	s.active--
	if s.active == 0 && s.stop != nil {
		close(s.stop.drained)
	}
}

func (s *modelStopState) begin() (*modelStopOperation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return s.stop, false
	}
	op := &modelStopOperation{
		drained: make(chan struct{}),
		done:    make(chan struct{}),
	}
	if s.active == 0 {
		close(op.drained)
	}
	s.stop = op
	return op, true
}

func (s *modelStopState) finish(op *modelStopOperation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != op {
		return
	}
	s.stop = nil
	close(op.done)
}
