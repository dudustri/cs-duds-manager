package api

import (
	"crypto/rand"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// nameTTL: a claim the browser has not renewed (it does on every page load) for this long is free again.
const nameTTL = 24 * time.Hour

// badNameChar matches what the web client's cleanName strips: the engine's `name "..."` command can't carry these.
var badNameChar = regexp.MustCompile(`[\x00-\x1f\x7f"';\\]`)

func validName(n string) bool {
	return n != "" && n == strings.TrimSpace(n) && utf8.ValidString(n) && utf8.RuneCountInString(n) <= 31 && !badNameChar.MatchString(n)
}

// names keeps every player name unique: each is held by the random token of the browser that claimed it, one name
// per token. In memory only, like rooms: a restart frees every name.
type names struct {
	mu sync.Mutex
	m  map[string]nameClaim // lower-cased name -> claim
}

type nameClaim struct {
	token string
	seen  time.Time
}

func (n *names) freeFor(name, token string, now time.Time) bool {
	c, ok := n.m[strings.ToLower(name)]
	return !ok || (token != "" && c.token == token) || now.Sub(c.seen) > nameTTL
}

func (n *names) available(name, token string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.freeFor(name, token, time.Now())
}

// claim gives name to token (a new token when empty) and frees the name that token held before.
func (n *names) claim(name, token string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	if !n.freeFor(name, token, now) {
		return "", false
	}
	if token == "" {
		token = rand.Text()
	}
	// shortcut: a full scan per claim, fine for a LAN party's few hundred names; index by token if that grows
	for k, c := range n.m {
		if c.token == token || now.Sub(c.seen) > nameTTL {
			delete(n.m, k)
		}
	}
	n.m[strings.ToLower(name)] = nameClaim{token, now}
	return token, true
}

// nameToken reads `Authorization: Name <token>`.
func nameToken(r *http.Request) string {
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if scheme != "Name" {
		return ""
	}
	return tok
}

func (s *Server) nameAvailable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, 400, "bad_name", "names are 1-31 characters, without quotes, semicolons or backslashes")
		return
	}
	writeJSON(w, 200, map[string]bool{"available": s.names.available(name, nameToken(r))})
}

func (s *Server) claimName(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name string }
	if !decode(w, r, &body) {
		return
	}
	if !validName(body.Name) {
		writeErr(w, 400, "bad_name", "names are 1-31 characters, without quotes, semicolons or backslashes")
		return
	}
	if !s.nameLm.allow(clientIP(r)) {
		writeErr(w, 429, "rate_limited", "too many name changes, slow down")
		return
	}
	token, ok := s.names.claim(body.Name, nameToken(r))
	if !ok {
		writeErr(w, 409, "name_taken", "someone already plays under that name")
		return
	}
	writeJSON(w, 200, map[string]string{"name": body.Name, "token": token})
}
