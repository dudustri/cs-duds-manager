package room

import (
	"net/netip"
	"testing"
)

func TestParseStatusAddr(t *testing.T) {
	st, err := ParseStatus("map     :  de_dust2 at: 0 x, 0 y, 0 z\n" +
		"#      name userid uniqueid frag time ping loss adr\n" +
		"# 1 \"a\" 7 STEAM_0:1:5 0 00:09 41 0 127.10.0.1:27005\n" +
		"# 2 \"Bot\" 9 BOT 0 00:09 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Players[0].Addr; got != netip.MustParseAddr("127.10.0.1") {
		t.Fatalf("human addr %v", got)
	}
	if got := st.Players[1].Addr; got.IsValid() {
		t.Fatalf("bot addr %v", got)
	}
}
