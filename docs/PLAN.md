# Plan

CS Duds runs Counter-Strike 1.6 in a browser tab, on a machine you own. It is meant for you and your
friends: open a link, pick a room, play.

## Goals

- Play from any modern browser. No install, no plugin, no Steam account.
- Run the whole thing with one `docker compose up`, first on a laptop, later on a small server.
- Build everything from official, pinned sources. No third-party "browser CS" projects are run.

## Main choices

### Players never need Steam

The game server is ReHLDS with ReUnion, which accepts players without Steam. The server files come
from an anonymous SteamCMD download during the Docker build.

### The browser runs the real engine

The client is Xash3D FWGS compiled to WebAssembly, with the CS 1.6 client on top. We compile both
ourselves from the official FWGS repositories, which are git submodules in `third_party/` pinned to one
commit each. No prebuilt engine files are used: FWGS warns that third-party builds often carry malware,
and they publish no browser build of their own.

- The Docker build compiles them with the official Emscripten image, pinned by digest.
- Upstream's browser build is old, so it needs small fixes. Each fix is a short patch in
  `engine/patches/`, so every change to upstream code can be read and reviewed.
- `engine/net.js` sends the engine's network traffic to our WebRTC transport.
- Only what the page needs is built. No server code, bots, training maps or touch controls.

### WebRTC carries the game traffic

The game speaks UDP, and browsers can not open raw UDP sockets. So the browser opens one WebRTC data
channel to the Go backend, and the backend forwards each packet to the room's server.

- The data channel is unreliable and unordered, like UDP, so the game feels the same as a normal
  client.
- A player can only reach a room that exists. The relay is never an open proxy.
- Each player gets their own local address on the server side, so every player has their own id.

### One server process per room

Creating a room starts a new game server on its own port. The number of ports in `ROOM_PORTS` is the
number of rooms. A room with no players stops after `ROOM_IDLE_TIMEOUT` (one hour by default).

### Game files are packed at build time

The build packs the game files into one base bundle plus one bundle per map, and checks that every
file a map needs is there.

- The browser downloads the base bundle once, then each map the first time a room needs it.
- Everything is cached in the browser and checked with sha256.
- Community maps that miss a model or texture are left out. Missing sounds or skies are fine.

## What is built

| Area | What works |
|---|---|
| Lobby | Quick play, server list with filters and card view, players page, create page |
| Rooms | Create, join from a list or an invite link, host controls, room options per mode |
| In game | Team picker outside the game, full screen, mouse lock, overlay with players, bots and votes |
| Modes | Classic, deathmatch, free for all, GunGame, zombie, aim, AWP, knife, surf, climb |
| Maps | About 245 maps. Surf, climb, knife, aim and AWP maps only appear in their own mode |
| Sounds | Kill announcer (first blood, headshot, multi kills, killing sprees), GunGame sounds |
| Safety | Rate limits per address, a cap of live rooms per address, host and admin tokens |
| Tests | Go unit and API tests, tests against the real stack, browser tests with Playwright |

## Where things come from

Every download is pinned by version and sha256 in `backend/docker/pins.txt`. The engine sources are
pinned by git commit in `third_party/`, and every base image by digest.

| Part | Source |
|---|---|
| Server files | SteamCMD, app 90, anonymous |
| ReHLDS, ReGameDLL, Metamod-R, ReUnion | Official GitHub releases |
| AMX Mod X | Official 1.9 release |
| GunGame | Plugin source, compiled during the build |
| Our plugins | Chat votes, kill announcer, climb timer and zombie, in `server-config/amxx/` |
| Community maps | A CS 1.6 server mirror on GitHub, pinned to one commit. Only map, sound, model and texture files are used |
| Browser engine | Xash3D FWGS and cs16-client, compiled from the official FWGS repositories (git submodules) |

## Next

1. Deploy to a server: pack the images into one file, copy it over and start it there.
2. Embed the game in a personal website with an iframe.
3. Add jailbreak and deathrun once their maps are available.
4. Add a Silvio Santos voice pack for the announcer once the sound files are available.
5. Take lobby pictures for the remaining maps with `web/scripts/map-shots`.

## Legal note

The server software and the browser engine are open source. The CS 1.6 game files (maps, models,
sounds) belong to Valve and are not free to redistribute, and serving them to browsers counts as
redistribution. Keep the site private, behind an unlisted link or a password, for you and your
friends.
