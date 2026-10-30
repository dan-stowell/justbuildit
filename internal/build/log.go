package build

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// tailBytes bounds the in-memory tail buffer kept for each build log.
const tailBytes = 64 << 10

// logWriter writes a build log to w up to limit bytes, then drops further
// output (after a truncation notice). It always keeps the most recent output
// in memory so the tail reflects the end of the build even when truncated.
type logWriter struct {
	mu        sync.Mutex
	w         io.Writer
	limit     int64
	written   int64
	truncated bool
	tail      []byte
	err       error
}

func newLogWriter(w io.Writer, limit int64) *logWriter {
	return &logWriter{w: w, limit: limit, tail: make([]byte, 0, 2*tailBytes)}
}

// keep appends p to the tail buffer, which never grows past 2*tailBytes:
// when full, its last tailBytes are copied to the front.
func (l *logWriter) keep(p []byte) {
	if len(p) >= tailBytes {
		p = p[len(p)-tailBytes:]
		l.tail = l.tail[:0]
	}
	if len(l.tail)+len(p) > cap(l.tail) {
		n := tailBytes - len(p)
		l.tail = l.tail[:copy(l.tail, l.tail[len(l.tail)-n:])]
	}
	l.tail = append(l.tail, p...)
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.keep(p)

	if l.err != nil || l.truncated {
		return len(p), nil
	}
	chunk := p
	if room := l.limit - l.written; int64(len(chunk)) > room {
		chunk = chunk[:room]
		l.truncated = true
	}
	n, err := l.w.Write(chunk)
	l.written += int64(n)
	if err != nil {
		l.err = err
		return len(p), nil
	}
	if l.truncated {
		fmt.Fprintf(l.w, "\n[justbuildit: log truncated at %d bytes; log_tail in the build result still shows the end]\n", l.limit)
	}
	// Never fail the runner because of log trouble.
	return len(p), nil
}

// Tail returns the last n lines of output.
func (l *logWriter) Tail(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return lastLines(l.tail, n)
}

func lastLines(b []byte, n int) string {
	if len(b) > tailBytes {
		b = b[len(b)-tailBytes:]
	}
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	i := end
	for lines := 0; i > 0; i-- {
		if b[i-1] == '\n' {
			lines++
			if lines == n {
				break
			}
		}
	}
	return string(bytes.ToValidUTF8(b[i:], []byte("�")))
}
