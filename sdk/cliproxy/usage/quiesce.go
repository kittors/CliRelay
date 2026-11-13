package usage

// WithDrained waits until all admitted publications (including observers and
// overflow handlers), queued records, and plugin dispatches have finished. It
// then pauses admission while fn runs, and resumes it even if fn panics. It does
// not stop or start the dispatcher, so the manager remains reusable.
//
// Producers should be stopped before draining: sustained traffic can delay the
// quiescent point. Recursive Publish is allowed while draining, but fn must not
// publish, and WithDrained must not be called from this manager's publication or
// plugin callbacks (those callbacks are themselves work it would wait for).
func (m *Manager) WithDrained(fn func()) {
	if m == nil {
		if fn != nil {
			fn()
		}
		return
	}
	m.mu.Lock()
	for m.paused || m.activePublishers != 0 || m.dispatching || len(m.queue) != 0 {
		m.cond.Wait()
	}
	m.paused = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.paused = false
		m.cond.Broadcast()
		m.mu.Unlock()
	}()
	if fn != nil {
		fn()
	}
}

func (m *Manager) beginPublish() {
	m.mu.Lock()
	for m.paused {
		m.cond.Wait()
	}
	m.activePublishers++
	m.mu.Unlock()
}

func (m *Manager) endPublish() {
	m.mu.Lock()
	m.activePublishers--
	m.cond.Broadcast()
	m.mu.Unlock()
}

// WithDrainedDefault applies the reusable publication barrier to the default
// manager. See Manager.WithDrained for callback and producer requirements.
func WithDrainedDefault(fn func()) { DefaultManager().WithDrained(fn) }
