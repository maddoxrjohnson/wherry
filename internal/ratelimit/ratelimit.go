// Package ratelimit is a token bucket per client, shared by the router's
// request limit and the dashboard's login limit.
package ratelimit

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Limiter is a token bucket per client. Each key (an IP) has a bucket that
// refills at rate tokens per second up to burst; a request takes one token
// or is refused. Idle buckets are swept when the map reaches maxBuckets,
// since a bucket that has been idle long enough to refill is
// indistinguishable from a new one. If it is still full, a new key is
// refused: a client with many addresses must not be able to grow the map
// without limit, and keys already in it keep working.
type Limiter struct {
	rate, burst float64

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	maxBuckets = 10000

	// sweepInterval spaces out sweeps of a full map. A sweep walks every
	// bucket under the lock, so a flood of new keys must not run one each.
	sweepInterval = time.Second

	// ipv6ClientBits is the prefix an IPv6 client is keyed by. One host
	// usually owns a whole /64, so a bucket per address would be no limit
	// at all. A client with a shorter prefix still holds many /64s, which
	// is why the map has its own cap.
	ipv6ClientBits = 64
)

// New returns a limiter refilling at rate tokens per second, which may be
// below 1, up to burst.
func New(rate float64, burst int) *Limiter {
	return &Limiter{rate: rate, burst: float64(burst), buckets: make(map[string]*bucket)}
}

// Allow takes a token for key at time now and reports whether there was one.
func (l *Limiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			if now.Sub(l.lastSweep) >= sweepInterval {
				l.lastSweep = now
				l.sweep(now)
			}
			if len(l.buckets) >= maxBuckets {
				return false
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have been idle long enough to be full again.
func (l *Limiter) sweep(now time.Time) {
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
}

// ClientIP turns a request's RemoteAddr into a limiter key: the peer's
// address without the port, or its /64 for IPv6, for the router's limit as
// much as the login's. An address that does not parse comes back as it is.
// The router is the edge, so X-Forwarded-For is never trusted here.
func ClientIP(remoteAddr string) string {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return remoteAddr
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, _ := addr.Prefix(ipv6ClientBits) // 64 is always valid for IPv6
	return prefix.String()
}
