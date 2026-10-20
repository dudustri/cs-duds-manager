# CS Duds

Counter-Strike 1.6 that you play in a browser tab. You host it yourself. Players need no install and no Steam account.

## How it works

```mermaid
flowchart LR
    B["Browser<br/>game engine in WebAssembly"]
    C["Caddy<br/>web page and game files"]
    G["Go backend<br/>lobby API and network relay"]
    S["Game servers<br/>one per room"]
    B -- "page, maps, sounds (HTTP)" --> C
    C -- "/api" --> G
    B -- "game traffic (WebRTC)" --> G
    G -- "game traffic (UDP)" --> S
```

1. The browser loads the page and the game files from Caddy, and keeps them cached.
2. The lobby lists the rooms. Creating a room starts a new game server on the machine.
3. When you join, the browser opens a WebRTC connection to the Go backend. The backend
   passes every game packet to that room's server and back.

## Run it

You need Linux, Docker with Compose, about 6 GB of disk and Chrome or Firefox.

    docker compose up --build
    xdg-open http://localhost

The first build downloads the game server files and the community maps. It takes a while.
Later builds use the cache. To change a setting, copy `.env.example` to `.env` and edit it.

## What you get

- 10 modes: classic, deathmatch, free for all, GunGame, zombie, aim, AWP, knife, surf and climb.
- About 245 maps: the stock maps plus community maps. Special maps only show up in their own mode.
- A lobby with quick play, a server list, a players page and a page to create a server.
- Host controls in the game overlay: kick, bots and map votes. Chat votes with `rtv` and `/votekick`.

## Admin

    printf '%s' 'my-password' | docker compose run --rm -T backend hash-password >> .env
    docker compose up -d

Then open http://localhost/admin to stop rooms and use host controls in any room.

## Tests

    scripts/test          # quick tests, no Docker
    scripts/test stack    # starts the real stack and plays it in a headless browser

## More

- `docs/PLAN.md`: what the project is, the choices made and what comes next.
- `docs/ARCHITECTURE.md`: diagrams of every part.
