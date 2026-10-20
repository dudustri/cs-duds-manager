// Command cs-backend is the whole backend: relay, API and room manager in one process.
//
//	cs-backend [serve]     run the server, configured from the environment
//	cs-backend hash-password   read a password on stdin, print ADMIN_PASSWORD_HASH='<bcrypt>'
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cs-duds/backend/internal/api"
	"cs-duds/backend/internal/relay"
	"cs-duds/backend/internal/room"
)

const listenAddr = "127.0.0.1:8080" // Caddy's upstream; never published

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if err := hashPassword(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "hash-password:", err)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := configFromEnv(os.Getenv, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cs-backend:", err)
		os.Exit(2)
	}
	if err := serve(cfg, log); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func hashPassword(in io.Reader, out io.Writer) error {
	pw, err := io.ReadAll(io.LimitReader(in, 1024))
	if err != nil {
		return err
	}
	pw = []byte(strings.TrimRight(string(pw), "\r\n"))
	if len(pw) == 0 {
		return errors.New("empty password")
	}
	h, err := bcrypt.GenerateFromPassword(pw, 12)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "ADMIN_PASSWORD_HASH='%s'\n", h)
	return err
}

// configFromEnv fails fast on any bad value. Paths default to the backend image layout.
func configFromEnv(getenv func(string) string, log *slog.Logger) (api.Config, error) {
	var errs []error
	str := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	num := func(k string, def, min, max int) int {
		v := str(k, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max {
			errs = append(errs, fmt.Errorf("%s=%q: want an integer in %d..%d", k, v, min, max))
			return def
		}
		return n
	}
	dur := func(k string, def, min time.Duration) time.Duration {
		v := str(k, def.String())
		d, err := time.ParseDuration(v)
		if err != nil || d < min {
			errs = append(errs, fmt.Errorf("%s=%q: want a duration >= %s", k, v, min))
			return def
		}
		return d
	}

	ice := num("ICE_PORT", 27101, 1024, 65535)
	ports, err := parsePorts(str("ROOM_PORTS", "27015-27020"), ice)
	if err != nil {
		errs = append(errs, err)
	}
	var pub netip.Addr
	if v := getenv("PUBLIC_IP"); v != "" {
		if pub, err = netip.ParseAddr(v); err != nil || !pub.Is4() {
			errs = append(errs, fmt.Errorf("PUBLIC_IP=%q: want an IPv4 address", v))
		}
	} else {
		pub = detectPublicIP(log)
	}
	hlds := str("HLDS_DIR", "/opt/hlds")
	cfg := api.Config{
		Rooms: room.Config{
			HLDSDir: hlds, HLDSBin: str("HLDS_BIN", "./hlds_linux"), Ports: ports,
			IdleTimeout:  dur("ROOM_IDLE_TIMEOUT", 10*time.Minute, 10*time.Second),
			StatusEvery:  dur("STATUS_EVERY", 5*time.Second, 100*time.Millisecond),
			ReadyTimeout: dur("READY_TIMEOUT", 30*time.Second, time.Second),
			Log:          log,
		},
		ManifestPath: str("MANIFEST_PATH", "/opt/cs/manifest.json"),
		ModesDir:     str("MODES_DIR", hlds+"/cstrike/modes"),
		Relay: relay.Config{
			ICEPort: ice, PublicIP: pub,
			IdleTimeout:       dur("SESSION_IDLE_TIMEOUT", 60*time.Second, time.Second),
			SessionLimitPerIP: num("SESSION_LIMIT_PER_IP", 16, 1, 1<<16),
		},
		CreateLimitBurst: num("CREATE_LIMIT_BURST", 6, 1, 1<<20),
		CreateLimitEvery: dur("CREATE_LIMIT_EVERY", 5*time.Minute, time.Millisecond),
		LoginLimitBurst:  num("LOGIN_LIMIT_BURST", 5, 1, 1<<20),
		LoginLimitEvery:  dur("LOGIN_LIMIT_EVERY", 12*time.Second, time.Millisecond),
		RoomsPerIP:       num("ROOMS_PER_IP", 3, 0, 1<<16),
	}
	if h := getenv("ADMIN_PASSWORD_HASH"); h != "" {
		if _, err := bcrypt.Cost([]byte(h)); err != nil {
			errs = append(errs, errors.New("ADMIN_PASSWORD_HASH is not a bcrypt hash (single-quote it in .env: it contains $)"))
		}
		cfg.AdminPasswordHash = []byte(h)
	}
	return cfg, errors.Join(errs...)
}

// parsePorts reads "a-b" (or a single "a") and checks the sentinel -sport (port+10000) fits.
func parsePorts(spec string, ice int) ([]int, error) {
	as, bs, _ := strings.Cut(spec, "-")
	if bs == "" {
		bs = as
	}
	a, err1 := strconv.Atoi(as)
	b, err2 := strconv.Atoi(bs)
	if err1 != nil || err2 != nil || a < 1024 || b < a || b-a >= 64 || b+10000 > 65535 {
		return nil, fmt.Errorf("ROOM_PORTS=%q: want a-b with 1024 <= a <= b, b-a < 64, b+10000 <= 65535", spec)
	}
	if ice >= a && ice <= b {
		return nil, fmt.Errorf("ICE_PORT=%d lies inside ROOM_PORTS=%s", ice, spec)
	}
	ports := make([]int, 0, b-a+1)
	for p := a; p <= b; p++ {
		ports = append(ports, p)
	}
	return ports, nil
}

// detectPublicIP asks the kernel which local IPv4 would route to the outside (a UDP "dial" sends
// nothing). Firefox drops loopback ICE candidates, so loopback is only the last resort.
func detectPublicIP(log *slog.Logger) netip.Addr {
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		defer c.Close()
		if ap, err := netip.ParseAddrPort(c.LocalAddr().String()); err == nil && ap.Addr().Is4() {
			return ap.Addr()
		}
	}
	log.Warn("no outbound IPv4 route; advertising 127.0.0.1 (Firefox will not connect). Set PUBLIC_IP.")
	return netip.MustParseAddr("127.0.0.1")
}

func serve(cfg api.Config, log *slog.Logger) error {
	s, err := api.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	srv := &http.Server{Addr: listenAddr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", listenAddr, "public_ip", cfg.Relay.PublicIP, "ice_port", cfg.Relay.ICEPort, "room_ports", len(cfg.Rooms.Ports))
	select {
	case err = <-errc:
	case <-ctx.Done():
		log.Info("shutting down")
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = srv.Shutdown(sc); err != nil {
			srv.Close() // a create blocked on hlds readiness must not hold the shutdown
		}
		err = nil
	}
	s.Close() // stops every hlds child before the process (and its container) exits
	return err
}
