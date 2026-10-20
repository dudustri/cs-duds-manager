# syntax=docker/dockerfile:1
# Stages: fetch -> hlds -> addons -> gungame -> gobuild -> assets -> webbuild -> server -> backend -> caddy.
# Every third-party download goes through scripts/fetch and backend/docker/pins.txt (name, url, sha256).
# Every base image is pinned by digest. Targets: `backend` and `caddy` (see compose.yaml).

ARG DEBIAN=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

FROM ${DEBIAN} AS fetch
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates unzip \
 && rm -rf /var/lib/apt/lists/*
COPY scripts/fetch /usr/local/bin/fetch
COPY backend/docker/pins.txt /etc/pins.txt
ENV PINS=/etc/pins.txt

# The stock HLDS tree: the one content source for the server and (via assets) the browser.
FROM fetch AS hlds
RUN apt-get update \
 && apt-get install -y --no-install-recommends lib32gcc-s1 lib32stdc++6 \
 && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /opt/steamcmd /opt/hlds \
 && fetch steamcmd /tmp/steamcmd.tgz \
 && tar -xzf /tmp/steamcmd.tgz -C /opt/steamcmd \
 && rm /tmp/steamcmd.tgz
# App 90 needs several passes (the first ones fail with "Missing configuration" or a partial update):
# repeat the validating update until SteamCMD reports a full install.
RUN set -eu; ok=; \
    for i in 1 2 3 4 5 6 7 8; do \
      /opt/steamcmd/steamcmd.sh +force_install_dir /opt/hlds +login anonymous \
        +app_set_config 90 mod cstrike +app_update 90 -beta steam_legacy validate +quit 2>&1 | tee /tmp/steam.log; \
      if grep -q "Success! App '90' fully installed" /tmp/steam.log; then ok=1; break; fi; \
    done; \
    [ -n "$ok" ] || { echo "SteamCMD never reached a clean install of app 90" >&2; exit 1; }; \
    test -f /opt/hlds/hlds_linux -a -d /opt/hlds/cstrike -a -d /opt/hlds/valve; \
    test -f /opt/steamcmd/linux32/steamclient.so; \
    rm -rf /opt/hlds/steamapps /tmp/steam.log; \
    echo 10 > /opt/hlds/steam_appid.txt; \
    echo "stock HLDS tree sha256: $(cd /opt/hlds && find . -type f | sort | xargs sha256sum | sha256sum | cut -d' ' -f1)"

# Server add-ons, laid out as an overlay on the HLDS root. No ReAPI: nothing here uses it.
FROM fetch AS addons
RUN mkdir -p /dl /overlay/cstrike /overlay/valve
WORKDIR /dl
RUN for n in rehlds regamedll metamod-r reunion amxx-base amxx-cstrike bot-profiles; do fetch "$n" "$n.pkg"; done \
 && unzip -q rehlds.pkg 'bin/linux32/*' -d rehlds && cp -a rehlds/bin/linux32/. /overlay/ \
 && unzip -q regamedll.pkg 'bin/linux32/cstrike/*' -d regamedll && cp -a regamedll/bin/linux32/cstrike/. /overlay/cstrike/ \
 && unzip -q bot-profiles.pkg 'cstrike/*' -d /overlay \
 && unzip -q metamod-r.pkg 'addons/*' -d /overlay/cstrike \
 && unzip -q reunion.pkg 'addons/reunion/*' reunion.cfg -d /overlay/cstrike \
 && tar -xzf amxx-base.pkg -C /overlay/cstrike \
 && tar -xzf amxx-cstrike.pkg -C /overlay/cstrike \
 && chmod +x /overlay/hlds_linux \
 && test -s /overlay/cstrike/BotProfile.db \
 && test -f /overlay/engine_i486.so -a -f /overlay/cstrike/dlls/cs.so -a -f /overlay/cstrike/addons/metamod/metamod_i386.so \
 && test -f /overlay/cstrike/addons/reunion/reunion_mm_i386.so -a -f /overlay/cstrike/addons/amxmodx/dlls/amxmodx_mm_i386.so \
 && rm -rf /dl

# AMXX plugins compiled here (amxxpc is a 32-bit binary): GunGame 2.13c from the vendored source, and the
# our own chat-vote plugin (server-config/amxx/cs_duds_votes.sma).
FROM addons AS gungame
RUN apt-get update \
 && apt-get install -y --no-install-recommends lib32gcc-s1 lib32stdc++6 \
 && rm -rf /var/lib/apt/lists/*
COPY server-config/amxx/gungame.sma /overlay/cstrike/addons/amxmodx/scripting/gungame.sma
COPY server-config/amxx/gungame.txt /overlay/cstrike/addons/amxmodx/data/lang/gungame.txt
WORKDIR /overlay/cstrike/addons/amxmodx/scripting
RUN ./amxxpc gungame.sma -o../plugins/gungame.amxx \
 && test -s ../plugins/gungame.amxx
COPY server-config/amxx/cs_duds_votes.sma ./cs_duds_votes.sma
RUN ./amxxpc cs_duds_votes.sma -o../plugins/cs_duds_votes.amxx \
 && test -s ../plugins/cs_duds_votes.amxx && rm cs_duds_votes.sma
COPY server-config/amxx/cs_duds_announcer.sma ./cs_duds_announcer.sma
RUN ./amxxpc cs_duds_announcer.sma -o../plugins/cs_duds_announcer.amxx \
 && test -s ../plugins/cs_duds_announcer.amxx && rm cs_duds_announcer.sma
# The plugin precaches these sounds, so the server tree needs them.
RUN mkdir -p /overlay/cstrike/sound/gungame \
 && for n in brass_bell knife_level levelup lostlead nade_level takenlead tiedlead triple welcome; do \
      fetch "gg-$n" "/overlay/cstrike/sound/gungame/gg_$n.wav"; \
    done
RUN mkdir -p /overlay/cstrike/sound/misc \
 && for n in $(awk '$1 ~ /^qs-/ { sub(/^qs-/, "", $1); print $1 }' "$PINS"); do \
      fetch "qs-$n" "/overlay/cstrike/sound/misc/$n.wav"; \
    done \
 && test -f /overlay/cstrike/sound/misc/wickedsick.wav

FROM golang:1.27@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS gobuild
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/ ./cmd/cs-backend ./cmd/package-assets

# The packager runs on the STOCK tree (before any overlay). A missing reference fails the build here.
FROM hlds AS assets
COPY --from=gobuild /out/package-assets /usr/local/bin/package-assets
COPY server-config/maps.txt /tmp/maps.txt
# the plugins' sounds (GunGame, announcer) go into the browser's base bundle too
COPY --from=gungame /overlay/cstrike/sound/ /opt/hlds/cstrike/sound/
RUN package-assets -content /opt/hlds -maps /tmp/maps.txt -out /out

FROM node:24@sha256:14078204b6bb268c107ceff6d3c43f45a888105e5c2b18b14c32b3409d93a1b0 AS webbuild
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
COPY web/vendor ./vendor
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

# The assembled server tree: stock + overlay + our config.
FROM hlds AS server
COPY --from=gungame /overlay/ /opt/hlds/
COPY server-config/server.cfg /opt/hlds/cstrike/server.cfg
COPY server-config/metamod-plugins.ini /opt/hlds/cstrike/addons/metamod/plugins.ini
COPY server-config/modes/ /opt/hlds/cstrike/modes/
COPY server-config/reunion.cfg /tmp/reunion.override
COPY --from=assets /out/mapcycle.txt /opt/hlds/cstrike/mapcycle.txt
WORKDIR /opt/hlds/cstrike
RUN set -eu; \
    # metamod becomes the game DLL; it finds the real one (ReGameDLL's dlls/cs.so) by itself
    sed -i 's#^gamedll_linux .*#gamedll_linux "addons/metamod/metamod_i386.so"#' liblist.gam; \
    grep -q '^gamedll_linux "addons/metamod/metamod_i386.so"' liblist.gam; \
    # ReUnion: the stock file with our keys replaced in place; the salt is random per build
    salt=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'); \
    sed -i "s/@SALT@/$salt/" /tmp/reunion.override; \
    awk -F'[ \t]*=[ \t]*' 'NR==FNR { if ($0 !~ /^#/ && NF >= 2) kv[$1] = $2; next } \
         ($1 in kv) { print $1 " = " kv[$1]; seen[$1] = 1; next } { print } \
         END { for (k in kv) if (!(k in seen)) { print "missing key " k > "/dev/stderr"; exit 1 } }' \
      /tmp/reunion.override reunion.cfg > /tmp/reunion.cfg; \
    mv /tmp/reunion.cfg reunion.cfg; rm /tmp/reunion.override; \
    grep -q "^SteamIdHashSalt = $salt\$" reunion.cfg; \
    # core.ini's amxx_plugins would override the per-room `+localinfo amxx_plugins`; drop it (AMXX defaults to the same file)
    sed -i '/^amxx_plugins[[:space:]]/d;/^amxx_configsdir[[:space:]]/d' addons/amxmodx/configs/core.ini; \
    ! grep -q '^amxx_plugins\|^amxx_configsdir' addons/amxmodx/configs/core.ini; \
    # same for the per-room configs dir (the backend gives each room its own users.ini)
    # zBot on, no automatic bots: the host adds them from the overlay (BotProfile.db comes from the overlay)
    sed -i 's/^bot_enable .*/bot_enable "1"/' game_init.cfg; \
    printf '\nbot_quota "0"\nbot_join_after_player "0"\nbot_chatter "off"\n' >> game_init.cfg; \
    grep -q '^bot_enable "1"' game_init.cfg; \
    # the map votes (mapsmenu, mapchooser, rtv) offer the catalog only
    cp mapcycle.txt addons/amxmodx/configs/maps.ini; \
    # per-mode AMXX plugin list = stock plugins.ini (admin.amxx + adminvote.amxx included) + the mode's extras
    mkdir -p addons/amxmodx/configs/modes; \
    for d in modes/*/; do \
      m=$(basename "$d"); \
      { cat addons/amxmodx/configs/plugins.ini; printf '\ncs_duds_votes.amxx\ncs_duds_announcer.amxx\n'; [ ! -f "$d/plugins.txt" ] || cat "$d/plugins.txt"; } > "addons/amxmodx/configs/modes/$m.ini"; \
      grep -q '^admin.amxx' "addons/amxmodx/configs/modes/$m.ini"; \
      grep -q '^adminvote.amxx' "addons/amxmodx/configs/modes/$m.ini"; \
      grep -q '^cs_duds_votes.amxx' "addons/amxmodx/configs/modes/$m.ini"; \
      grep -q '^cs_duds_announcer.amxx' "addons/amxmodx/configs/modes/$m.ini"; \
    done; \
    grep -q '^gungame.amxx' addons/amxmodx/configs/modes/gungame.ini; \
    rm -rf addons/amxmodx/scripting

FROM ${DEBIAN} AS backend
# iproute2: the soak applies netem with `tc` inside this container.
RUN apt-get update \
 && apt-get install -y --no-install-recommends lib32gcc-s1 lib32stdc++6 iproute2 ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 10001 --shell /usr/sbin/nologin cs \
 && mkdir -p /home/cs/.steam/sdk32 /opt/cs \
 && chown -R cs:cs /home/cs/.steam
# ReHLDS fatals without Steam GS init: it looks for steamclient.so in ~/.steam/sdk32.
COPY --from=hlds --chown=cs:cs /opt/steamcmd/linux32/steamclient.so /home/cs/.steam/sdk32/steamclient.so
COPY --from=server --chown=cs:cs /opt/hlds /opt/hlds
COPY --from=assets /out/manifest.json /opt/cs/manifest.json
COPY --from=gobuild /out/cs-backend /usr/local/bin/cs-backend
ENV HLDS_DIR=/opt/hlds MANIFEST_PATH=/opt/cs/manifest.json LD_LIBRARY_PATH=/opt/hlds
WORKDIR /opt/hlds
USER cs
ENTRYPOINT ["cs-backend"]

FROM caddy:2.11@sha256:685ae3d7c34d9b5f8caeddc5d14051db7bbd88ff1f5a2fa0c16bd8ca328fb7af AS caddy
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=webbuild /web/dist/ /srv/
COPY --from=assets /out/assets/ /srv/assets/
COPY --from=assets /out/manifest.json /srv/manifest.json
