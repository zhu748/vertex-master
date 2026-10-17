package vertex

import (
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

var errStreamIdleTimeout = errors.New("upstream stream idle timeout") //nolint:gochecknoglobals // Sentinel error.

// Only time a blocked upstream Read. Time spent delivering a chunk is downstream
// backpressure and must not be charged to the node's upstream idle budget.
type idleStreamReader struct {
	body         io.Reader
	closeBody    func()
	timeout      time.Duration
	firstTimeout time.Duration
	seenData     bool
	mu           sync.Mutex
	generation   uint64
	reading      bool
	expired      bool
}

func (r *idleStreamReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.expired {
		r.mu.Unlock()
		return 0, errStreamIdleTimeout
	}
	r.reading = true
	timeout := r.timeout
	if !r.seenData && r.firstTimeout > 0 {
		timeout = r.firstTimeout
	}
	r.generation++
	generation := r.generation
	timer := time.AfterFunc(timeout, func() {
		r.mu.Lock()
		if !r.reading || r.generation != generation {
			r.mu.Unlock()
			return
		}
		r.expired = true
		r.mu.Unlock()
		r.closeBody()
	})
	r.mu.Unlock()
	n, err := r.body.Read(p)
	r.mu.Lock()
	r.reading = false
	r.seenData = r.seenData || n > 0
	timer.Stop()
	if r.expired {
		err = errStreamIdleTimeout
	}
	r.mu.Unlock()
	return n, err
}

// Generation can be silent before the first byte, especially for media. Keep
// the reference's family-specific allowance while the HTTP total timeout still
// provides the outer bound.
func streamIdleTimeouts(model string, baseSeconds int) (first, between time.Duration) {
	base := time.Duration(baseSeconds) * time.Second
	model = strings.ToLower(model)
	switch {
	case strings.Contains(model, "image"), strings.Contains(model, "virtual-try-on"):
		return 4 * base, 2 * base
	case strings.Contains(model, "tts"), strings.HasPrefix(model, "lyria-"):
		return 3 * base, 2 * base
	default:
		return 2 * base, base
	}
}
