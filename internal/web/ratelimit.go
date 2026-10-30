package web

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiter is a per-client token bucket.
type limiter struct {
	rate  rate.Limit
	burst int

	mu        sync.Mutex
	clients   map[string]*clientLimit
	lastSweep time.Time
}

type clientLimit struct {
	lim  *rate.Limiter
	seen time.Time
}

func newLimiter(r rate.Limit, burst int) *limiter {
	return &limiter{rate: r, burst: burst, clients: map[string]*clientLimit{}}
}

func (l *limiter) allow(client string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		for k, c := range l.clients {
			if now.Sub(c.seen) > 10*time.Minute {
				delete(l.clients, k)
			}
		}
		l.lastSweep = now
	}
	c := l.clients[client]
	if c == nil {
		c = &clientLimit{lim: rate.NewLimiter(l.rate, l.burst)}
		l.clients[client] = c
	}
	c.seen = now
	return c.lim.AllowN(now, 1)
}
