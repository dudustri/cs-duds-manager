package room

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Status struct {
	Map        string
	MaxPlayers int
	Players    []Player
}

type Player struct {
	Slot     int           `json:"-"`
	UserID   int           `json:"userid"`
	Name     string        `json:"name"`
	UniqueID string        `json:"steamid"` // "BOT" for bots
	Frags    int           `json:"frags"`
	Time     time.Duration `json:"-"`
	Ping     int           `json:"ping"`
	Loss     int           `json:"-"`
	Addr     string        `json:"-"` // never leaves the backend
	Bot      bool          `json:"bot"`
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
		case strings.HasPrefix(line, "players"):
			if _, v, ok := strings.Cut(line, "("); ok {
				st.MaxPlayers, _ = strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "max)")))
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
	f := strings.Fields(line[q2+1:])
	slot, err1 := strconv.Atoi(strings.TrimSpace(line[1:q1]))
	if err1 != nil || len(f) < 6 {
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
	var secs int
	for _, part := range strings.Split(f[3], ":") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return p, false, fmt.Errorf("status: malformed row %q", line)
		}
		secs = secs*60 + n
	}
	p = Player{Slot: slot, UserID: nums[0], Name: line[q1+1 : q2], UniqueID: f[1], Frags: nums[1],
		Time: time.Duration(secs) * time.Second, Ping: nums[2], Bot: f[1] == "BOT"}
	p.Loss, _ = strconv.Atoi(f[5])
	if len(f) > 6 {
		p.Addr = f[6]
	}
	return p, true, nil
}
