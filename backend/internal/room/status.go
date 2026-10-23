package room

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type Status struct {
	Map     string
	Players []Player
}

type Player struct {
	UserID   int        `json:"userid"`
	Name     string     `json:"name"`
	UniqueID string     `json:"steamid"` // "BOT" for bots
	Frags    int        `json:"frags"`
	Ping     int        `json:"ping"`
	Bot      bool       `json:"bot"`
	Addr     netip.Addr `json:"-"` // where the player plays from: each relay session has its own; never published
}

func (s Status) Humans() int {
	n := 0
	for _, p := range s.Players {
		if !p.Bot {
			n++
		}
	}
	return n
}

// ParseStatus parses ReHLDS `status` output. Rows are parsed from the right of the quoted name
// (which may itself contain quotes and spaces); the first number is the slot, not the userid.
func ParseStatus(text string) (Status, error) {
	var st Status
	seenMap := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r\x00 ")
		switch {
		case strings.HasPrefix(line, "map"):
			if _, v, ok := strings.Cut(line, ":"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					st.Map, seenMap = f[0], true
				}
			}
		case strings.HasPrefix(line, "#") && strings.Count(line, `"`) >= 2:
			p, ok, err := parseRow(line)
			if err != nil {
				return Status{}, err
			}
			if ok {
				st.Players = append(st.Players, p)
			}
		}
	}
	if !seenMap {
		return Status{}, errors.New("status: no map line")
	}
	return st, nil
}

func parseRow(line string) (p Player, ok bool, err error) {
	q1, q2 := strings.Index(line, `"`), strings.LastIndex(line, `"`)
	// after the name: userid uniqueid frag time ping loss [adr]
	f := strings.Fields(line[q2+1:])
	if _, err := strconv.Atoi(strings.TrimSpace(line[1:q1])); err != nil || len(f) < 6 {
		return p, false, fmt.Errorf("status: malformed row %q", line)
	}
	if strings.HasPrefix(strings.ToLower(f[1]), "hltv") {
		return p, false, nil
	}
	nums := [3]int{}
	for i, s := range []string{f[0], f[2], f[4]} {
		if nums[i], err = strconv.Atoi(s); err != nil {
			return p, false, fmt.Errorf("status: malformed row %q", line)
		}
	}
	p = Player{UserID: nums[0], Name: line[q1+1 : q2], UniqueID: f[1], Frags: nums[1], Ping: nums[2], Bot: f[1] == "BOT"}
	if len(f) > 6 { // bots have no address
		if ap, err := netip.ParseAddrPort(f[6]); err == nil {
			p.Addr = ap.Addr()
		}
	}
	return p, true, nil
}
