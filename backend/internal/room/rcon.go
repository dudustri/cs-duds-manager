package room

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

var oob = []byte{0xff, 0xff, 0xff, 0xff}

// command can only be built by the constructors below: no request string ever reaches RCON.
type command struct{ line string }

func statusCmd() command             { return command{"status"} }
func kickCmd(userid int) command     { return command{fmt.Sprintf("kick #%d", userid)} }         // userid > 0 checked at the API
func cvarCmd(m Mode) command         { return command{m.probeCvar} }                             // reads the mode's probe cvar
func voteKickCmd(userid int) command { return command{fmt.Sprintf("amx_votekick #%d", userid)} } // userid checked against a fresh status
func botDifficultyCmd(d int) command { return command{fmt.Sprintf("bot_difficulty %d", d)} }     // 0-3 checked at the API
func botAddCmd(ct bool) command {
	if ct {
		return command{"bot_add_ct"}
	}
	return command{"bot_add_t"}
}
func botKickCmd() command      { return command{"bot_kick"} }
func reloadAdminsCmd() command { return command{"amx_reloadadmins"} }
func voteMapCmd(maps []Map) command {
	names := make([]string, len(maps))
	for i, m := range maps {
		names[i] = m.name
	}
	return command{"amx_votemap " + strings.Join(names, " ")}
}

type rcon struct {
	addr     netip.AddrPort
	password string // never serialized, never logged
}

var (
	errBadPassword  = errors.New("rcon: bad password")
	errBadChallenge = errors.New("rcon: bad challenge")
)

// exec runs one RCON command from its own UDP socket, so concurrent calls never read each other's replies.
func (c rcon) exec(ctx context.Context, cmd command) (string, error) {
	out, err := c.once(ctx, cmd)
	if errors.Is(err, errBadChallenge) {
		out, err = c.once(ctx, cmd)
	}
	return out, err
}

func (c rcon) once(ctx context.Context, cmd command) (string, error) {
	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, net.UDPAddrFromAddrPort(c.addr))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(2 * time.Second)
	}
	conn.SetDeadline(dl)
	defer context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })()

	buf := make([]byte, 65535)
	if _, err := conn.Write(append(bytes.Clone(oob), "challenge rcon\n"...)); err != nil {
		return "", err
	}
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(string(buf[:n]), string(oob)+"challenge rcon ")
	chal, perr := strconv.ParseUint(strings.Trim(rest, "\n\x00 "), 10, 32)
	if !ok || perr != nil {
		return "", errors.New("rcon: bad challenge reply")
	}
	if _, err := fmt.Fprintf(conn, "%srcon %d \"%s\" %s\n", oob, chal, c.password, cmd.line); err != nil {
		return "", err
	}
	var out strings.Builder
	for got := false; ; {
		n, err := conn.Read(buf)
		if err != nil {
			if got {
				break // quiet period (or deadline) after at least one packet: reply complete
			}
			return "", err
		}
		if p, ok := bytes.CutPrefix(buf[:n], append(bytes.Clone(oob), 'l')); ok {
			out.Write(p)
			got = true
			if q := time.Now().Add(80 * time.Millisecond); q.Before(dl) {
				conn.SetReadDeadline(q)
			}
		}
	}
	text := strings.ReplaceAll(out.String(), "\x00", "") // ReHLDS pads replies with NULs
	switch {
	case strings.Contains(text, "Bad rcon_password"):
		return "", errBadPassword
	case strings.Contains(text, "Bad challenge"):
		return "", errBadChallenge
	}
	return text, nil
}
