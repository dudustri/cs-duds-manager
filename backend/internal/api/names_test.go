package api_test

import (
	"encoding/json"
	"net/url"
	"testing"
)

// A name belongs to one browser at a time (case-insensitively); claiming another frees the old one.
func TestNames(t *testing.T) {
	h := start(t)
	claim := func(name, token string, status int, code string) string {
		t.Helper()
		hdr := map[string]string{}
		if token != "" {
			hdr["Authorization"] = "Name " + token
		}
		var got struct{ Name, Token string }
		json.Unmarshal(h.expect(t, "POST", "/api/names", map[string]string{"name": name}, hdr, status, code), &got)
		return got.Token
	}
	available := func(name, token string) bool {
		t.Helper()
		hdr := map[string]string{}
		if token != "" {
			hdr["Authorization"] = "Name " + token
		}
		var got struct{ Available bool }
		json.Unmarshal(h.expect(t, "GET", "/api/names/"+url.PathEscape(name), nil, hdr, 200, ""), &got)
		return got.Available
	}

	alice := claim("Alice", "", 200, "")
	if alice == "" {
		t.Fatal("no token")
	}
	claim("alice", "", 409, "name_taken")
	if available("ALICE", "") || !available("Alice", alice) || !available("bob", "") {
		t.Fatal("availability")
	}
	if claim("Alice", alice, 200, "") != alice { // renewing keeps the token
		t.Fatal("renew changed the token")
	}
	bob := claim("bob", "", 200, "")
	claim("Bob", alice, 409, "name_taken")
	if claim("Alice2", alice, 200, "") != alice || !available("Alice", bob) {
		t.Fatal("claiming a new name did not free the old one")
	}
	for _, bad := range []string{"", " pad", `a"b`, "a;b", `a\b`, "0123456789012345678901234567890x"} {
		claim(bad, "", 400, "bad_name")
	}
	h.expect(t, "GET", "/api/names/a%3Bb", nil, nil, 400, "bad_name")
}
