package server

import (
	"net"
	"sync"
	"time"
)

// failLimiter blocks clients that fail authentication too often: after
// `limit` failures within `window`, further attempts from the same IP are
// refused until the window ends.
type failLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	clients map[string]*failures
}

type failures struct {
	count int
	reset time.Time
}

func newFailLimiter(limit int, window time.Duration) *failLimiter {
	return &failLimiter{limit: limit, window: window, now: time.Now, clients: map[string]*failures{}}
}

func clientIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// blocked reports whether ip has used up its failures.
func (l *failLimiter) blocked(ip string) bool {
	if l == nil || l.limit <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.clients[ip]
	return f != nil && f.count >= l.limit && l.now().Before(f.reset)
}

// fail records a failed attempt from ip.
func (l *failLimiter) fail(ip string) {
	if l == nil || l.limit <= 0 {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.clients[ip]
	if f == nil || !now.Before(f.reset) {
		f = &failures{reset: now.Add(l.window)}
		l.clients[ip] = f
	}
	f.count++
	if len(l.clients) > 10000 {
		for k, v := range l.clients {
			if !now.Before(v.reset) {
				delete(l.clients, k)
			}
		}
	}
}
