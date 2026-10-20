package room

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A room's live sessions: the vote tokens they were handed and, for the host's own sessions, the
// source address that AMX Mod X trusts as an admin. The room is the only writer of its users file.
type sessions struct {
	mu     sync.Mutex
	tokens map[string]struct{}
	admins map[netip.Addr]struct{}
}

// hostAdminLine is a users.ini entry: reservation, kick, slay, map, vote, menu; IP-based, no password.
const hostAdminLine = `"%s" "" "bcefju" "de"`

func roomConfigsRel(id string) string { return "addons/amxmodx/rooms/" + id }

// mapcycleRel is the room's map cycle, relative to cstrike/. Short on purpose: hlds cuts long mapcyclefile values.
func mapcycleRel(id string) string { return "mapcycle_" + id + ".txt" }

// mkConfigs gives the room its own AMXX configs dir: symlinks to the stock files plus its own users.ini, and its
// mode's maps as maps.ini (AMXX map menus and votes) and a map cycle (+mapcyclefile: rtv, nextmap).
func (m *Manager) mkConfigs(r *Room) error {
	if err := os.MkdirAll(r.cfgDir, 0o755); err != nil {
		return err
	}
	stock, err := filepath.Abs(filepath.Join(m.cfg.HLDSDir, "cstrike", "addons/amxmodx/configs"))
	if err != nil {
		return err
	}
	es, _ := os.ReadDir(stock) // absent in tests
	for _, e := range es {
		if e.Name() == "users.ini" || e.Name() == "maps.ini" {
			continue
		}
		if err := os.Symlink(filepath.Join(stock, e.Name()), filepath.Join(r.cfgDir, e.Name())); err != nil {
			return err
		}
	}
	if len(r.maps) > 0 {
		list := []byte(strings.Join(r.maps, "\n") + "\n")
		cstrike := filepath.Join(m.cfg.HLDSDir, "cstrike")
		for _, f := range []string{filepath.Join(r.cfgDir, "maps.ini"), filepath.Join(cstrike, mapcycleRel(r.ID))} {
			if err := os.WriteFile(f, list, 0o644); err != nil {
				return err
			}
		}
	}
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	return r.writeUsers()
}

// writeUsers renders the admins into users.ini; the caller holds sess.mu.
func (r *Room) writeUsers() error {
	var b strings.Builder
	b.WriteString("; written by cs-backend: the host's own sessions in this room\n")
	for _, a := range slices.SortedFunc(func(yield func(netip.Addr) bool) {
		for a := range r.sess.admins {
			if !yield(a) {
				return
			}
		}
	}, netip.Addr.Compare) {
		fmt.Fprintf(&b, hostAdminLine+"\n", a)
	}
	tmp := filepath.Join(r.cfgDir, ".users.ini.tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.cfgDir, "users.ini"))
}

// Session is a player's vote token for this room; it counts only while attached to a relay session.
type Session struct {
	Token string
	r     *Room
	host  bool
}

// NewSession makes the token up front. A host session also becomes an admin of this room once attached.
func (r *Room) NewSession(host bool) *Session {
	return &Session{Token: base64.RawURLEncoding.EncodeToString(randBytes(32)), r: r, host: host}
}

// Attach registers the session as live, holding the source address src, and returns the function that
// ends it (idempotent). It fits relay.Target.Attach.
func (s *Session) Attach(src netip.Addr) (end func()) {
	r := s.r
	r.sess.mu.Lock()
	if r.sess.tokens == nil {
		r.sess.tokens, r.sess.admins = map[string]struct{}{}, map[netip.Addr]struct{}{}
	}
	r.sess.tokens[s.Token] = struct{}{}
	if s.host {
		r.sess.admins[src] = struct{}{}
		r.adminsChanged()
	}
	r.sess.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.sess.mu.Lock()
			defer r.sess.mu.Unlock()
			delete(r.sess.tokens, s.Token)
			if _, ok := r.sess.admins[src]; ok {
				delete(r.sess.admins, src)
				r.adminsChanged()
			}
		})
	}
}

// adminsChanged rewrites users.ini (caller holds sess.mu) and asks the server to reload it.
func (r *Room) adminsChanged() {
	if err := r.writeUsers(); err != nil {
		if r.ctx.Err() == nil { // a stopping room's dir is already gone
			r.log.Error("users.ini", "room", r.ID, "err", err)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
		defer cancel()
		r.rcon.exec(ctx, reloadAdminsCmd())
	}()
}

// HasSession reports whether token belongs to a live session of this room.
func (r *Room) HasSession(token string) bool {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	for t := range r.sess.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return true
		}
	}
	return false
}
