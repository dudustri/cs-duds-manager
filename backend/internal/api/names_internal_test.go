package api

import (
	"testing"
	"time"
)

func TestNameClaimExpires(t *testing.T) {
	n := names{m: map[string]nameClaim{}}
	tok, _ := n.claim("carol", "")
	n.m["carol"] = nameClaim{tok, time.Now().Add(-nameTTL - time.Minute)}
	if _, ok := n.claim("Carol", ""); !ok {
		t.Fatal("an expired claim still holds the name")
	}
}
