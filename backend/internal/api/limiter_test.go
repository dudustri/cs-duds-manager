package api

import (
	"net/netip"
	"testing"
	"time"
)

func TestLimiterSharesIPv6Slash64(t *testing.T) {
	l := newLimiter(1, time.Hour)
	if !l.allow(netip.MustParseAddr("2001:db8:1:2::1")) {
		t.Fatal("first request refused")
	}
	if l.allow(netip.MustParseAddr("2001:db8:1:2:ffff::9")) {
		t.Fatal("another address in the same /64 got a fresh bucket")
	}
	if !l.allow(netip.MustParseAddr("2001:db8:1:3::1")) || !l.allow(netip.MustParseAddr("198.51.100.1")) {
		t.Fatal("a different /64 or an IPv4 address shared the bucket")
	}
}

func TestLimiterMapStaysBounded(t *testing.T) {
	l := newLimiter(2, time.Hour)
	for i := range maxLimiterEntries + 5000 {
		l.allow(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	if len(l.m) > maxLimiterEntries+1 {
		t.Fatalf("%d buckets", len(l.m))
	}
}
