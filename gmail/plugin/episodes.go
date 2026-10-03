package plugin

import "sync"

// episodes logs each condition once when it starts and again only after it
// has cleared, so a failure repeated on every refresh is one line.
type episodes struct {
	logf func(format string, args ...any)

	mu   sync.Mutex
	open map[string]bool
}

// note records the outcome of the condition named key: a failure opens its
// episode, logged with format and args if it was not open; nil closes it.
func (e *episodes) note(key string, err error, format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		delete(e.open, key)
		return
	}
	if e.open[key] {
		return
	}
	if e.open == nil {
		e.open = map[string]bool{}
	}
	e.open[key] = true
	e.logf(format, args...)
}
