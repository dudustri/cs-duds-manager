# CS 1.6 in the browser — plan

Goal: self-hosted Counter-Strike 1.6, playable from a browser tab (like play-cs.com),
embeddable on a personal website, with a server browser and a curated map/mode rotation.

- **Local first:** the whole stack runs on a laptop with one `docker compose up`. You can
  play it at `http://localhost` before any VM exists.
- **Bundle deploy:** the same stack is packaged into one versioned tarball, copied to the
  VM and started there. Local and VM differ only in `.env`.

Status: facts verified 2026-10-19 (see `.claude-pipeline/browser-cs-local/grounding.md`).
The implementation design for Phases 0–4 is `.claude-pipeline/browser-cs-local/design.md`,
and the diagrams are in `ARCHITECTURE.md`.

Decision: **players never need Steam.** No Steam account, client or login for anyone
playing. The server runs ReHLDS + ReUnion (`sv_lan 0`) and accepts non-Steam clients. Its
HLDS base files come from an anonymous SteamCMD download (app 90, `-beta steam_legacy`)
inside our own Dockerfile.

Decision: **all code is our own, built from first-party sources.** No third-party
"browser CS" repos or Docker images are run. Every binary comes from its upstream
project's official release, pinned by version and sha256 in `backend/docker/pins.txt`.

## Prior art (inspiration only — nothing here is run or copied wholesale)

play-cs.com is closed source and behind a Cloudflare bot check. Its public behaviour:
- a server list with a "Connect" button and no install
- game files downloaded once on the first visit, then cached
- some servers gated by skill level

Small open-source clones show how such a thing can be built. They are unvetted, so we take
only ideas from them:
- **[oharashane/cs16-web](https://github.com/oharashane/cs16-web):** WebRTC data channels
  to a Go (pion) relay to ReHLDS + ReUnion. One loopback address per player. A
  background-tab keepalive.
- **[Elkhan-Isayev/cs16-web](https://github.com/Elkhan-Isayev/cs16-web)** and
  **[x8BitRain/webXash](https://github.com/x8BitRain/webXash):** variants on top of the
  (now removed) upstream webxash3d-fwgs.
- [HN thread](https://news.ycombinator.com/item?id=23497236): an older browser CS that used
  only WebSocket (TCP) for game traffic was criticised for laggy netcode, so we use WebRTC.

## Supply chain (verified)

| Component | Version / source | Verify |
|---|---|---|
| HLDS base | SteamCMD anonymous, `app_set_config 90 mod cstrike`, `app_update 90 -beta steam_legacy validate` (repeat until clean; branch frozen since 2020) | hash the installed tree |
| steamcmd bootstrap | `steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz` | sha256 `cebf0046…71e7c` |
| ReHLDS | 3.15.0.896, `rehlds/ReHLDS` releases | `.asc` (key `6354…FB67`) + GitHub digest |
| ReGameDLL_CS | 5.30.0.814, `rehlds/ReGameDLL_CS` | `.asc` + digest |
| Metamod-R | 1.3.0.149, `rehlds/Metamod-R` | no published digest; we pin our own sha256 |
| ReUnion | 0.2.0.34, `rehlds/ReUnion` (**open source, GPL-3.0** since 2024) | `.asc` + digest; needs `SteamIdHashSalt` ≥ 16 chars |
| AMX Mod X | 1.9.0.5303 stable, `alliedmodders/amxmodx` GitHub releases (base + cstrike) | digest |
| GunGame | 2.13c `.sma` from `fysiks1/amxx-gungame_cstrike`, compiled in the build | pin commit |
| Xash3D-FWGS WASM | npm `xash3d-fwgs@1.2.2` (deprecated, still downloadable, SLSA provenance) → vendored | sha512 integrity + sha256 |
| cs16-client | `cs16-client@0.1.2`, **unpublished from npm 2026-09-08**; tarball from registry.npmmirror.com → vendored | sha512 integrity + sha256 |
| pion/webrtc | v4 Go module | `go.sum` |
| Caddy | official `caddy:2.11` image | pinned by digest |

Not needed:
- `hlsdk-portable`: cs16-client ships its own client/menu/server wasm.
- ReAPI: no plugin in Phases 0–4 uses it.

## Top risks (all have a Phase 0 test)

1. **The engine's upstream is gone.**
   - `yohimik/webxash3d-fwgs` and its forks were removed.
   - Upstream FWGS can't build for Emscripten (the PRs were closed unmerged).
   - Mitigation: vendor the two tarballs now. The fallback source build uses the pinned
     SHAs (FWGS `f85aa0c8f7`, Velaron `2490c5e9b6`) or the `ololoken` forks.
2. **The prebuilt engine has limits.**
   - Its wasm heap is fixed at 256 MiB, which may not fit large maps.
   - It is driven by requestAnimationFrame. Hidden tabs drop to about 1 fps and the server
     times them out. The plan is a worker-timer rAF shim while the tab is hidden; this is
     untested.
   - If either fails, the follow-up is a source build with a bigger heap and a per-frame
     export.
3. **WebRTC reachability.**
   - Firefox rejects loopback ICE candidates by default, so `PUBLIC_IP` defaults to the
     host's LAN IP, never 127.0.0.1.
   - A VM needs UDP 27101 open. Networks that block UDP would need TURN; add it only if
     someone actually hits this.
4. **Content mismatch.** If the server precaches a file it lacks, it crashes on map load. If
   the client lacks a file, the join breaks. The packager runs inside the Docker build and
   fails it on any missing reference. `sv_allowdownload 0`.
5. ~~SharedArrayBuffer / COOP/COEP~~ **Resolved.** The build uses no pthreads and no
   SharedArrayBuffer, so the headers aren't needed. Embedding in an iframe needs only
   `allow="fullscreen"` and CSP `frame-ancestors`.

## Architecture

The diagrams are in `ARCHITECTURE.md`. Two containers on `network_mode: host`:

1. **`backend`.** One Go binary (`cs-backend`) plus the server files. It runs the relay, the
   API and the room manager, and the `hlds_linux` rooms run as its child processes.
   - **Signaling:** one HTTP request, `POST /api/rooms/{id}/session` (SDP offer in, answer
     out), to an ICE-lite pion relay on UDP 27101.
   - **Game traffic:** one unreliable, unordered data channel (`game`). One message is one
     datagram, with no header and no address field.
   - **Destination:** the room id in the URL is the *only* thing a client chooses. The relay
     resolves it to the live room's `127.0.0.1:port` and sends from a **connected** UDP
     socket bound to a fresh `127.10.x.y`. This has three effects:
     - it can't become an open proxy, because the set of live rooms is the allowlist;
     - every player is a distinct IP and gets a distinct ReUnion `STEAM_` id;
     - the engine always connects to the virtual address `192.0.2.1:27015`, and the client
       adapter tags every reply with it.
   - **Rooms:**
     - one supervisor goroutine per room;
     - a single teardown path that crashes, the idle reaper and DELETE all go through;
     - RCON (hand-rolled GoldSrc UDP) is the only server protocol;
     - `status` feeds the list, the players view and the reaper;
     - no A2S in production.
2. **`caddy`.** Serves the client page and the content-addressed asset bundles, and
   proxies `/api/*`. A domain in `SITE_ADDRESS` gets automatic Let's Encrypt.
3. **Browser client (`web/`).** Our own page on top of the vendored engine.
   - **Bundles:** a base pk3 plus one pk3 per map, which FWGS mounts natively. They are
     cached in CacheStorage, and every catalog map is present before the first connect, so
     vote-map and GunGame map changes always have their files.
   - **Background tabs:** a hidden-tab rAF shim keeps the engine ticking.

## Local development

```
docker compose up --build          # first build pulls HLDS via SteamCMD (~1 GB), cached after
xdg-open http://localhost          # create room → play; second window → connect from the list
scripts/test                       # fast tier: Go + node tests, no Docker
scripts/test stack                 # real compose stack: Go black-box + Playwright (Chromium/SwiftShader)
scripts/test soak                  # + 30-min, 10-peer soak under netem (container-scoped, NET_ADMIN)
```

- `.env` is optional; copy `.env.example` only to change a default.
- netem on `lo` applies per direction, so `delay 40ms loss 1%` gives about 80 ms RTT and
  2 % loss.

## Deployment (Phase 5 — next /deliver)

- **`scripts/bundle`** produces `dist/cs-duds-<git-sha>.tar.gz`:
  - `docker save` of the backend and caddy images. Retag them to local names and use
    `pull_policy: never`, so the VM never tries to pull.
  - `compose.yaml`
  - no `.env` and no secrets
- **`scripts/deploy <host>`:**
  1. `scp` the bundle to `/opt/cs-duds/releases/<sha>/`.
  2. Over ssh, `docker load` it, repoint the `current` symlink, and run
     `docker compose up -d` with the VM's own `.env`.
  3. Rollback means repointing the symlink and running `up -d` again.
- **VM prerequisites:** Docker. Open TCP 80/443 and UDP 27101. Point DNS at the VM.
- The images already contain the content, since the packager runs in the build, so the
  bundle is about 1–2 GB.

## Build order

| Phase | Work | Done when (automated) |
|---|---|---|
| 0 | Server stack image, relay, minimal client | Two Chromium windows play each other on de_dust2 through the relay with distinct STEAM_ ids (`phase0-two-players.spec.ts`) |
| 1 | Relay hardening | Non-room ports unreachable; per-session sources; limits; 10 peers stable for 30 min at 80 ms/2 % (`api_test.go`, `soak_test.go`) |
| 2 | Asset packaging + client cache | Packager rejects a map with a missing file; a fresh profile downloads once and reload is instant (`package_test.go`, `assets.spec.ts`) |
| 3 | Room list + connect | Connect from the list works end to end (`connect-from-list.spec.ts`) |
| 4 | Room manager + host controls | Create → join → vote map → kick → idle reap (`host-controls.spec.ts`, `api_test.go`) |
| 5 | Bundle + deploy | Works from a phone on mobile data via `https://cs.yourdomain/`; rollback tested once |
| 6 | Embed on personal site | Plays inside the iframe, including pointer lock and fullscreen |

Phases 0–4 are the first `/deliver` (`/deliver --slug browser-cs-local`); 5–6 come after.

## Rooms, lobby & host controls

Decision: **multiple isolated rooms**. Each room is its own `hlds_linux` process with a
port from `ROOM_PORTS` (default 27015–27020). The size of that range is the room cap.

### API (behind Caddy at `/api`)

| Endpoint | Auth | Notes |
|---|---|---|
| `GET /api/catalog` | — | maps (from `server-config/maps.txt`) and modes |
| `GET /api/rooms` | — | id, map, mode, players (humans), maxPlayers |
| `POST /api/rooms` `{map, mode}` | per-IP rate limit | 201 + `hostToken` only after RCON answers **and** the mode's probe cvar is right. Errors: 400 unknown map/mode, 429, 503 full, 504 start_failed |
| `DELETE /api/rooms/{id}` | admin | returns after the process exits |
| `GET /api/rooms/{id}/players` | host / admin | fresh RCON `status` |
| `POST /api/rooms/{id}/kick` `{userid}` | host / admin | `kick #<userid>`; no name strings, so `;` and `"` in names are harmless |
| `POST /api/rooms/{id}/vote-map` `{maps}` | host / admin | 1–4 catalog maps → `amx_votemap …`; 409 if a vote is running or too soon |
| `POST /api/rooms/{id}/session` | per-IP session limit | WebRTC signaling (SDP) |
| `POST /api/admin/login` | rate-limited | bcrypt hash in `.env`; HMAC cookie: `HttpOnly`, `SameSite=Strict`, `Secure` when served over https |

### Launch (per room)

```
./hlds_linux -game cstrike -port P -sport P+10000 -maxplayers 16 +ip 127.0.0.1
  +rcon_password <random> +sv_rcon_condebug 0
  +localinfo amxx_plugins addons/amxmodx/configs/modes/<mode>.ini
  +mapchangecfgfile modes/<mode>/mode.cfg +map <map>
```

- **Mode plugins:** use `+localinfo amxx_plugins`. AMXX auto-loads every
  `configs/plugins-*.ini`, so per-mode files must never be named that way.
- **Mode cvars:** ReGameDLL re-runs `game.cfg` on every map load, and `+mapchangecfgfile`
  re-applies the mode's cvars after it. Exactly one line per mode cfg carries `// probe`;
  the backend checks that cvar before reporting a room ready.
- **RCON password:** `+sv_rcon_condebug 0` must come before `+map`, otherwise ReHLDS echoes
  the RCON password into its log.
- **RCON bans:** repeated bad RCON passwords make hlds ban the source address. Only the
  backend talks RCON, from 127.0.0.1.

### Host identity, map voting, kick, site admin

- **Host actions:** every host action is a web-UI button → backend → RCON. The room's
  `hostToken` is kept in the creator's `localStorage`. No in-game admin is needed.
- **Map voting:** uses stock `adminvote.amxx`. It fails while another vote is running,
  within `amx_vote_delay`, or below the 0.40 ratio.
- **Kick:** the player list comes from RCON `status`. Its first number is the slot, not
  the userid, so it is parsed from the right.
- **Site admin:** one password, which can stop any room and passes every host check. Rooms
  and admin sessions are in memory and vanish on restart.
- **Idle cleanup:** rooms with 0 humans for `ROOM_IDLE_TIMEOUT` (default 10 min) stop
  themselves.

## Game content: maps & modes

### Modes in the first delivery

| Mode | How |
|---|---|
| Classic | stock; `mp_forcerespawn 0 // probe` |
| Deathmatch | ReGameDLL cvars `mp_forcerespawn 1 // probe`, `mp_round_infinite 1` (no CSDM, which needs a prebuilt module) |
| Gun Game | GunGame 2.13c compiled from `.sma` in the build; `gg_enabled 1 // probe` |

### Later

| Mode | Server-side needs |
|---|---|
| Zombie | Zombie Plague 4.3 (GPL `.sma`) + its models/sounds |
| Surf | `surf_` maps, `sv_airaccelerate 100`; optional timer plugin |
| Bhop / KZ | auto-bhop or KZ timer plugin |
| Fun (fy_, aim_, awp_, 35hp_) | maps + DM cvars |

Plugins are third-party code. Read the `.sma` source and compile it in the build; never
use prebuilt `.amxx` files.

### Map list

- **First delivery:** the stock maps from app 90, curated in `server-config/maps.txt`: de_dust2,
  de_dust, de_inferno, de_nuke, de_train, de_aztec, cs_assault, cs_italy, cs_office.
- **Later, when sourced:**
  - community favorites: cs_rio, fy_pool_day, de_rats, aim_map, aim_awp, aim_headshot,
    fy_ak47, fy_chaves, fy_simpsons
  - fun pack: fy_iceworld, awp_india, 35hp_2, kz/bhop, surf_, zm_
- Custom maps go through a content overlay into the same build stage, so the packager
  checks them like stock maps.

### Killstreak announcer (later)

An AMX Mod X multi-kill announcer plugin + its `.wav` files. The `.wav` files must be in
the content tree so they ship in the client bundle.

### Asset bundling

- **Single source:** the same SteamCMD layer feeds the server's `cstrike/` and the
  packager, which runs inside `docker build`.
- **Bundles:** `base.pk3` (everything except maps) plus one pk3 per map, with WADs stored
  uncompressed, and a `manifest.json` with a sha256 per bundle.
- **Validation:** the packager parses each BSP for wads, sky, entity-referenced
  models/sprites/sounds, `.res` files and external textures. A missing file fails the
  build, and it lists all of them.
- **Client cache:** bundles are cached in CacheStorage by content-addressed URL and
  verified with sha256.

## Legal note

ReHLDS, ReGameDLL, ReUnion, AMX Mod X and Xash3D FWGS are open source. HLDS base files are a
free anonymous download. CS1.6 game content (models/textures/sounds/maps) is not
redistributable under Valve's EULA, and serving it in the web bundle *is* redistribution.
play-cs.com operates in that gray zone. A private link for you and friends is low-risk;
keep the page behind an unlisted URL or basic auth until you decide.
