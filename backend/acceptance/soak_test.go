//go:build soak

// 10 pion peers send A2S_INFO at 20 Hz to one real room through the relay for SOAK_DURATION
// (default 30m); >= 90% must be answered and every session must stay open.
//
// Impairment: the test applies `tc qdisc ... netem delay 40ms loss 1%` itself by running tc INSIDE the
// backend container: `scripts/test soak` brings the stack up with compose.soak.yaml (adds cap_add: [NET_ADMIN]
// to the backend; the backend image needs iproute2), so the host needs no sudo. Under network_mode: host
// that qdisc sits on the host's lo, so it is removed again in a defer (and by scripts/test's trap).
// Env: STACK_URL (required), SOAK_DURATION, SOAK_NETEM=off (skip netem), SOAK_COMPOSE (docker compose command
// prefix, default "docker compose -p cs-duds-test -f compose.yaml -f compose.soak.yaml", run from the repo root).
package acceptance

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"cs-duds/backend/internal/testpeer"
)

func netem(t *testing.T, args string) error {
	compose := os.Getenv("SOAK_COMPOSE")
	if compose == "" {
		compose = "docker compose -p cs-duds-test -f compose.yaml -f compose.soak.yaml"
	}
	cmd := exec.Command("sh", "-c", compose+" exec -T backend tc "+args)
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("tc %s: %v\n%s", args, err, out)
	}
	return err
}

func TestSoak(t *testing.T) {
	base := stackURL(t)
	dur := 30 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("SOAK_DURATION")); err == nil {
		dur = d
	}
	if os.Getenv("SOAK_NETEM") != "off" {
		if err := netem(t, "qdisc replace dev lo root netem delay 40ms loss 1%"); err != nil {
			t.Fatal("cannot apply netem; start the stack with compose.soak.yaml (NET_ADMIN) or set SOAK_NETEM=off")
		}
		defer netem(t, "qdisc del dev lo root")
	}
	room := createRoom(t, base, "de_dust2", "classic")
	const n = 10
	type stat struct{ sent, got atomic.Int64 }
	stats := make([]*stat, n)
	peers := make([]*testpeer.Peer, n)
	ctx, stop := context.WithTimeout(context.Background(), dur)
	defer stop()
	for i := range peers {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		p, resp, err := testpeer.Dial(dctx, base, room.ID)
		cancel()
		if err != nil {
			t.Fatalf("peer %d: %v %v", i, resp, err)
		}
		defer p.Close()
		peers[i], stats[i] = p, &stat{}
		go func() {
			for {
				if _, err := p.Recv(context.Background()); err != nil {
					return
				}
				stats[i].got.Add(1)
			}
		}()
	}
	query := append([]byte{0xff, 0xff, 0xff, 0xff}, "TSource Engine Query\x00"...)
	tick := time.NewTicker(50 * time.Millisecond) // 20 Hz
	defer tick.Stop()
	for run := true; run; {
		select {
		case <-ctx.Done():
			run = false
		case <-tick.C:
			for i, p := range peers {
				select {
				case <-p.Closed():
					t.Fatalf("peer %d: session closed before the soak ended", i)
				default:
				}
				if p.Send(query) == nil {
					stats[i].sent.Add(1)
				}
			}
		}
	}
	time.Sleep(3 * time.Second) // let in-flight replies land
	for i, s := range stats {
		sent, got := s.sent.Load(), s.got.Load()
		if sent == 0 || float64(got) < 0.9*float64(sent) {
			t.Errorf("peer %d: %d of %d A2S_INFO answered (%.1f%%), want >= 90%%", i, got, sent, 100*float64(got)/float64(max(sent, 1)))
		}
		select {
		case <-peers[i].Closed():
			t.Errorf("peer %d: session closed", i)
		default:
		}
	}
}
