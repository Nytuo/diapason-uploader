package server

import (
	"net"
	"sync"
	"time"
)

const (
	maxAuthFailures = 8
	failureWindow   = time.Minute
	blockDuration   = time.Minute
)

// authLimiter throttles repeated bad-token attempts per source IP so the
// bearer token can't be brute-forced at line rate.
type authLimiter struct {
	mu      sync.Mutex
	entries map[string]*authEntry
}

type authEntry struct {
	failures    int
	windowEnd   time.Time
	blockedTill time.Time
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{entries: map[string]*authEntry{}}
}

func (l *authLimiter) key(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func (l *authLimiter) blocked(ip net.IP) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[l.key(ip)]
	if !ok {
		return false
	}
	return time.Now().Before(e.blockedTill)
}

func (l *authLimiter) fail(ip net.IP) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := l.key(ip)
	now := time.Now()
	e, ok := l.entries[key]
	if !ok || now.After(e.windowEnd) {
		e = &authEntry{windowEnd: now.Add(failureWindow)}
		l.entries[key] = e
	}
	e.failures++
	if e.failures >= maxAuthFailures {
		e.blockedTill = now.Add(blockDuration)
		e.failures = 0
		e.windowEnd = now.Add(failureWindow)
	}
	if len(l.entries) > 10000 {
		l.pruneLocked(now)
	}
}

// pruneLocked drops entries that are neither in an active failure window nor
// currently blocked, bounding memory use under sustained scanning.
func (l *authLimiter) pruneLocked(now time.Time) {
	for k, e := range l.entries {
		if now.After(e.windowEnd) && now.After(e.blockedTill) {
			delete(l.entries, k)
		}
	}
}

func (l *authLimiter) reset(ip net.IP) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, l.key(ip))
}
