package process

import "sync"

// ringBuffer is a goroutine-safe fixed-capacity line buffer. Once full, each
// Append overwrites the oldest line.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	start int // index of the oldest line
	count int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{lines: make([]string, capacity)}
}

// Append adds one line, evicting the oldest line when the buffer is full.
func (r *ringBuffer) Append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count < len(r.lines) {
		r.lines[(r.start+r.count)%len(r.lines)] = line
		r.count++
		return
	}
	r.lines[r.start] = line
	r.start = (r.start + 1) % len(r.lines)
}

// Last returns the most recent n lines in order (oldest first). n <= 0, or n
// larger than what is buffered, returns everything buffered.
func (r *ringBuffer) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > r.count {
		n = r.count
	}
	out := make([]string, n)
	for i := range out {
		out[i] = r.lines[(r.start+r.count-n+i)%len(r.lines)]
	}
	return out
}
