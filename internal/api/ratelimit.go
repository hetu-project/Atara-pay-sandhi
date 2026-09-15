package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter is a small in-memory brute-force guard for the admin login.
//
// It counts failed attempts per client key (IP) within a sliding window; after
// too many, that key is blocked for a cool-down. Success resets the key. This is
// process-local (fine for a single instance / internal tool); a multi-instance
// deployment behind a load balancer would want a shared store instead.
type loginLimiter struct {
	mu       sync.Mutex
	m        map[string]*loginAttempts
	maxFails int
	window   time.Duration
	block    time.Duration
	lastGC   time.Time
}

type loginAttempts struct {
	fails        int
	windowStart  time.Time
	blockedUntil time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		m:        map[string]*loginAttempts{},
		maxFails: 5,                // attempts allowed per window
		window:   15 * time.Minute, // window the count is measured over
		block:    15 * time.Minute, // cool-down once tripped
		lastGC:   time.Now(),
	}
}

// allow reports whether this key may attempt a login now. If blocked, retryAfter
// is how long until it may try again.
func (l *loginLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.gc(now)
	a := l.m[key]
	if a == nil {
		return true, 0
	}
	if now.Before(a.blockedUntil) {
		return false, time.Until(a.blockedUntil)
	}
	return true, 0
}

// fail records a failed attempt and blocks the key once it crosses the limit.
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a := l.m[key]
	if a == nil || now.Sub(a.windowStart) > l.window {
		a = &loginAttempts{windowStart: now}
		l.m[key] = a
	}
	a.fails++
	if a.fails >= l.maxFails {
		a.blockedUntil = now.Add(l.block)
	}
}

// reset clears a key after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

// gc drops stale entries so the map doesn't grow without bound. Called under lock.
func (l *loginLimiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < l.window {
		return
	}
	l.lastGC = now
	for k, a := range l.m {
		if now.After(a.blockedUntil) && now.Sub(a.windowStart) > l.window {
			delete(l.m, k)
		}
	}
}

// clientIP is the key for the limiter. It prefers the first X-Forwarded-For hop
// (set by a trusted reverse proxy) and falls back to the socket address.
//
// Note: X-Forwarded-For is client-spoofable if the backend is exposed directly.
// In production the backend should sit behind a proxy that overwrites this header.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
