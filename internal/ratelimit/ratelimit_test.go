package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := New(2, 4) // 2/s, burst 4
	now := time.Unix(1000, 0)
	for i := range 4 {
		if !l.Allow("a", now) {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	if l.Allow("a", now) {
		t.Fatal("fifth request in the same instant allowed")
	}
	if !l.Allow("b", now) {
		t.Fatal("another client should have its own bucket")
	}
	if !l.Allow("a", now.Add(500*time.Millisecond)) {
		t.Fatal("half a second refills one token at 2/s")
	}
	if l.Allow("a", now.Add(500*time.Millisecond)) {
		t.Fatal("and only one")
	}
	later := now.Add(time.Hour)
	for i := range 4 {
		if !l.Allow("a", later) {
			t.Fatalf("an idle bucket refills to the burst: request %d refused", i+1)
		}
	}
	if l.Allow("a", later) {
		t.Fatal("burst exceeded: an idle bucket refills to the burst, not beyond")
	}
}

func TestLimiterRateBelowOne(t *testing.T) {
	l := New(1.0/30, 2) // one token every 30 s
	now := time.Unix(1000, 0)
	for i := range 2 {
		if !l.Allow("a", now) {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	if l.Allow("a", now.Add(29*time.Second)) {
		t.Fatal("allowed before a whole token had refilled")
	}
	if !l.Allow("a", now.Add(30*time.Second)) {
		t.Fatal("30 seconds should refill one token")
	}
}

func TestLimiterSweepsIdleBuckets(t *testing.T) {
	l := New(1, 1)
	now := time.Unix(1000, 0)
	for i := range maxBuckets {
		l.Allow("ip"+string(rune(i)), now)
	}
	if len(l.buckets) != maxBuckets {
		t.Fatalf("%d buckets", len(l.buckets))
	}
	l.Allow("late", now.Add(time.Second)) // full map: the idle ones go
	if len(l.buckets) != 1 {
		t.Fatalf("%d buckets after the sweep, want 1", len(l.buckets))
	}
}

func fill(l *Limiter, now time.Time) {
	for i := range maxBuckets {
		l.Allow("ip"+strconv.Itoa(i), now)
	}
}

func TestLimiterFullMapRefusesNewKeys(t *testing.T) {
	l := New(1, 1)
	now := time.Unix(1000, 0)
	fill(l, now)
	later := now.Add(500 * time.Millisecond) // nothing has been idle for a whole refill
	if l.Allow("new", later) {
		t.Fatal("a new key was allowed into a full map")
	}
	if len(l.buckets) != maxBuckets {
		t.Fatalf("%d buckets, want %d", len(l.buckets), maxBuckets)
	}
	if !l.Allow("ip0", now.Add(time.Second)) {
		t.Fatal("a key already in the map stopped working")
	}
}

func TestLimiterThrottlesSweeps(t *testing.T) {
	l := New(10, 1) // idle after 100 ms
	now := time.Unix(1000, 0)
	fill(l, now)
	if l.Allow("new", now) { // this sweep finds nothing idle
		t.Fatal("a new key was allowed into a full map")
	}
	if l.Allow("new", now.Add(sweepInterval-time.Millisecond)) {
		t.Fatal("swept again before the interval was up")
	}
	if len(l.buckets) != maxBuckets {
		t.Fatalf("%d buckets, want no sweep", len(l.buckets))
	}
	if !l.Allow("new", now.Add(sweepInterval)) {
		t.Fatal("the sweep after the interval should have made room")
	}
	if len(l.buckets) != 1 {
		t.Fatalf("%d buckets after the sweep, want 1", len(l.buckets))
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"ipv4", "192.0.2.1:1234", "192.0.2.1"},
		{"ipv4 no port", "192.0.2.1", "192.0.2.1"},
		{"ipv4-mapped", "[::ffff:192.0.2.1]:1234", "192.0.2.1"},
		{"ipv6", "[2001:db8:1:2:3:4:5:6]:1234", "2001:db8:1:2::/64"},
		{"same /64", "[2001:db8:1:2:ffff::1]:99", "2001:db8:1:2::/64"},
		{"other /64", "[2001:db8:1:3::1]:1234", "2001:db8:1:3::/64"},
		{"ipv6 no port", "2001:db8:1:2::9", "2001:db8:1:2::/64"},
		{"ipv6 zone", "[fe80::1%eth0]:1234", "fe80::/64"},
		{"unparsable", "not an address", "not an address"},
		{"unparsable with port", "pipe:80", "pipe:80"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(tc.in); got != tc.want {
				t.Errorf("ClientIP(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
