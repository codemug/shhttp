package server

import (
	"testing"
	"time"
)

func TestFailLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newFailLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if l.blocked("1.2.3.4") {
			t.Fatalf("blocked after %d failures", i)
		}
		l.fail("1.2.3.4")
	}
	if !l.blocked("1.2.3.4") || l.blocked("5.6.7.8") {
		t.Fatal("limit not applied per IP")
	}
	now = now.Add(time.Minute)
	if l.blocked("1.2.3.4") {
		t.Fatal("still blocked after the window")
	}
	if clientIP("[::1]:5000") != "::1" || clientIP("10.0.0.1:80") != "10.0.0.1" {
		t.Fatal("clientIP")
	}
	var disabled *failLimiter
	disabled.fail("x")
	if disabled.blocked("x") {
		t.Fatal("nil limiter blocks")
	}
}
