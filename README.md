# cs-duds: Counter-Strike 1.6 in a browser tab (local stack)

Needs: Linux, Docker 29+ with Compose v5, about 5 GB of disk, Chrome or Firefox.

    docker compose up --build          # first build pulls HLDS via SteamCMD (~1 GB, ~10 min); cached after
    xdg-open http://localhost

`.env` is optional. Copy `.env.example` to `.env` only to change a default.

## Play

1. Pick a map and a mode, then "Create room". You are the room's host.
2. The page downloads the game once (about 240 MB with the stock maps), then caches it.
   The next visit loads from the cache.
3. You are in. A second player opens another window, finds the room in the list and clicks "Connect".

Modes: classic, deathmatch (ReGameDLL cvars) and gungame (GunGame 2.13c).
Maps: the list in `server-config/maps.txt`.

## Host controls

Shown only in the browser that created the room.

- Players: live list with "Kick".
- Vote map: pick 1-4 maps, players vote in-game.

A room with no humans for `ROOM_IDLE_TIMEOUT` (default 10 min) shuts itself down.
The number of rooms is the size of `ROOM_PORTS` (default 27015-27020: 6 rooms).

## Site admin

    printf '%s' 'my-password' | docker compose run --rm -T backend hash-password >> .env
    docker compose up -d

Open http://localhost/admin, log in, and you can stop any room and use host controls in any room.
The hash is single-quoted in `.env` because it contains `$`.

## Tests

One command: `scripts/test [fast|stack|soak]` (default `fast`; each tier includes the ones before it).

    scripts/test          # fast: Go unit + in-process black-box tests, web unit tests. No Docker.
    scripts/test stack    # + builds and starts the real stack, runs Go stack tests and Playwright
    scripts/test soak     # + 30-minute run: 10 pion peers against a real room under netem

The stack tiers use ports 80 and 27101 and the ports 27015-27022, so stop anything on them.
The soak adds `compose.soak.yaml` (`NET_ADMIN` for the backend) and applies and removes netem itself
with `tc` inside the container, so the host needs no sudo.

## Layout

| Path | What |
|---|---|
| `backend/` | Go: `cs-backend` (relay, API, room manager) and `package-assets` (client bundles) |
| `web/` | the page, the vendored engine and its tests |
| `server-config/` | `server.cfg`, ReUnion, metamod, `maps.txt`, per-mode cfgs, the vendored GunGame source |
| `backend/docker/pins.txt` | every third-party download: name, URL, sha256 (fetched by `scripts/fetch`) |
| `Dockerfile` | one build for the server tree, the browser bundles and the site |
| `compose.yaml`, `Caddyfile` | backend and Caddy on host networking |

Design notes: `ARCHITECTURE.md`.

## Notes

- Rootless Docker or Podman cannot bind :80. Set `SITE_ADDRESS=http://localhost:8000`.
- Firefox drops loopback ICE candidates, so the relay advertises this machine's LAN IP. It is
  auto-detected; set `PUBLIC_IP` to override it.
- Opening the site over `http://<LAN-IP>` from another device works, but that origin is not a
  secure context, so CacheStorage is unavailable and that device re-downloads the game every visit.
- Linux only: both containers use `network_mode: host`.
- The backend binds `127.0.0.1:8080` and each room binds `127.0.0.1:<port>`; only Caddy (:80) and the
  WebRTC port (UDP `ICE_PORT`) are reachable from outside.
