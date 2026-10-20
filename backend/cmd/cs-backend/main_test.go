package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

func TestDefaults(t *testing.T) {
	cfg, err := configFromEnv(env(nil), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rooms.Ports) != 6 || cfg.Rooms.Ports[0] != 27015 || cfg.Relay.ICEPort != 27101 || !cfg.Relay.PublicIP.IsValid() {
		t.Errorf("defaults: %+v", cfg)
	}
	if cfg.Rooms.IdleTimeout != time.Hour || cfg.CreateLimitBurst != 6 || cfg.LoginLimitEvery != 12*time.Second {
		t.Errorf("limits: %+v", cfg)
	}
}

func TestOverridesAndErrors(t *testing.T) {
	cfg, err := configFromEnv(env(map[string]string{
		"ROOM_PORTS": "27015-27022", "PUBLIC_IP": "10.1.2.3", "CREATE_LIMIT_EVERY": "1ms", "ROOM_IDLE_TIMEOUT": "45s",
	}), slog.Default())
	if err != nil || len(cfg.Rooms.Ports) != 8 || cfg.Relay.PublicIP.String() != "10.1.2.3" || cfg.CreateLimitEvery != time.Millisecond {
		t.Fatalf("%v %+v", err, cfg)
	}
	for k, v := range map[string]string{
		"ROOM_PORTS": "27020-27015", "ICE_PORT": "27016", "ROOM_IDLE_TIMEOUT": "1s", "PUBLIC_IP": "nope",
		"SESSION_LIMIT_PER_IP": "0", "ADMIN_PASSWORD_HASH": "$2a$bad", "READY_TIMEOUT": "soon",
	} {
		if _, err := configFromEnv(env(map[string]string{k: v}), slog.Default()); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}

func TestHashPassword(t *testing.T) {
	var out bytes.Buffer
	if err := hashPassword(strings.NewReader("hunter2\n"), &out); err != nil {
		t.Fatal(err)
	}
	h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(out.String()), "ADMIN_PASSWORD_HASH='"), "'")
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("hunter2")) != nil {
		t.Errorf("hash %q does not match", out.String())
	}
	if _, err := configFromEnv(env(map[string]string{"ADMIN_PASSWORD_HASH": h}), slog.Default()); err != nil {
		t.Error(err)
	}
}
