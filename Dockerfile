# syntax=docker/dockerfile:1
# Stages: fetch -> hlds -> addons -> gungame -> gobuild -> extramaps -> assets -> teamart -> engine -> webbuild -> server
#         -> backend -> caddy.
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
COPY server-config/amxx/cs_duds_announcer.sma server-config/amxx/cs_duds_timer.sma server-config/amxx/cs_duds_zombie.sma ./
RUN for p in cs_duds_announcer cs_duds_timer cs_duds_zombie; do \
      ./amxxpc "$p.sma" "-o../plugins/$p.amxx" && test -s "../plugins/$p.amxx" && rm "$p.sma" || exit 1; \
    done
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
# Community maps: a CS 1.6 server-tree mirror pinned to one commit (git verifies the content by its hash). Only
# data folders are fetched and only data file types kept (no plugins, configs or binaries); names are lowercased
# because the Linux server opens files case-sensitively. The packager offers every map whose files all resolve.
FROM ${DEBIAN} AS extramaps
ARG MAPS_MIRROR=https://github.com/hpoon/HLDS-CS1.6
ARG MAPS_COMMIT=5953bf494f28bcf60bcbe94ed1416654ba6ab6be
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/* \
 && git init -q /mirror && cd /mirror && git remote add origin "$MAPS_MIRROR" \
 && git sparse-checkout set --no-cone '/cstrike/maps/' '/cstrike/*.wad' '/cstrike/gfx/env/' '/cstrike/sound/' '/cstrike/models/' '/cstrike/sprites/' \
 && git fetch -q --depth 1 --filter=blob:none origin "$MAPS_COMMIT" && git checkout -q FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "$MAPS_COMMIT" \
 && mkdir -p /extra && cd cstrike \
 && find . -type f \( -iname '*.bsp' -o -iname '*.wad' -o -iname '*.wav' -o -iname '*.mdl' -o -iname '*.spr' -o -iname '*.tga' \
      -o -iname '*.bmp' -o -iname '*.res' -o -iname '*.txt' \) -exec cp --parents {} /extra/ \; \
 && cd /extra && find . -depth -name '*[A-Z]*' -execdir sh -c 'n=$(printf %s "$1" | tr A-Z a-z); [ -e "$n" ] || mv "$1" "$n"' _ {} \; \
 && rm -rf /mirror && test -f /extra/maps/cs_rio.bsp

FROM hlds AS assets
COPY --from=gobuild /out/package-assets /usr/local/bin/package-assets
COPY server-config/maps.txt /tmp/maps.txt
COPY server-config/maps-exclude.txt /tmp/maps-exclude.txt
COPY --from=extramaps /extra/ /extra/
# the plugins' sounds (GunGame, announcer) go into the browser's base bundle too
COPY --from=gungame /overlay/cstrike/sound/ /opt/hlds/cstrike/sound/
RUN package-assets -content /opt/hlds -maps /tmp/maps.txt -extra /extra -exclude /tmp/maps-exclude.txt -out /out

# The team picker shows the game's own team-menu characters (gfx/vgui/*.tga), converted for browsers.
FROM hlds AS teamart
RUN apt-get update && apt-get install -y --no-install-recommends imagemagick && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /out/teams \
 && for n in terror leet arctic guerilla urban gsg9 sas gign; do \
      convert "/opt/hlds/cstrike/gfx/vgui/$n.tga" -auto-orient -quality 85 "/out/teams/$n.webp"; \
    done \
 && test -s /out/teams/gign.webp

# The browser engine, compiled from source: Xash3D FWGS and cs16-client from the pinned submodules in third_party/
# (never prebuilt binaries), plus our build fixes in engine/patches/ and the WebRTC network glue engine/net.js.
# Only what the page loads is kept: the engine, its file system and WebGL 2 renderer, the CS client and menu, and
# the menu art. No server code, bots, training maps or touch controls.
FROM emscripten/emsdk:6.0.12@sha256:77638e6c215a65f25a704c0c831d5c2463a8c0468a58c6cb82489dbe621dfa05 AS engine
WORKDIR /src
COPY third_party/ third_party/
COPY engine/ engine/
RUN cd third_party/xash3d-fwgs && for p in /src/engine/patches/xash3d-fwgs/*.patch; do patch -p1 < "$p"; done \
 && cd ../cs16-client && for p in /src/engine/patches/cs16-client/*.patch; do patch -p1 < "$p"; done
RUN cd third_party/xash3d-fwgs \
 && XASH_BUILD_DATE="$(date -u '+%Y-%m-%d %H:%M:%S +0000')" \
    CC=emcc CXX=em++ AR=emar RANLIB=emranlib python3 ./waf configure -T release --emscripten --enable-gles3compat --disable-gl \
 && python3 ./waf build -j"$(nproc)"
RUN cd third_party/cs16-client \
 && emcmake cmake -S . -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_SERVER=OFF -DMAINUI_USE_STB=ON \
 && cmake --build build -j"$(nproc)"
RUN x=third_party/xash3d-fwgs/build && c=third_party/cs16-client/build && o=/out/engine \
 && mkdir -p $o/valve $o/cstrike/cl_dlls \
 && cp $x/engine/xash $o/xash.js && cp $x/engine/xash.wasm $x/filesystem/filesystem_stdio.so $x/ref/gl/libref_gles3compat.so $o/ \
 && cp $x/3rdparty/extras/extras.pk3 $o/valve/extras.pk3 \
 && cp $c/cl_dll/client_emscripten_wasm32.so $c/3rdparty/mainui_cpp/menu_emscripten_wasm32.so $o/cstrike/cl_dlls/ \
 && cd third_party/cs16-client/3rdparty/cs16client-extras \
 && python3 -c 'import os,sys,zipfile; z=zipfile.ZipFile(sys.argv[1],"w",zipfile.ZIP_DEFLATED); [z.write(os.path.join(d,f)) for d,_,fs in os.walk("gfx") for f in fs]' $o/cstrike/extras.pk3
# served under a hash of its files, so browsers may cache it forever and a rebuilt engine gets a new URL
RUN v=$(cd /out/engine && find . -type f | sort | xargs sha256sum | sha256sum | cut -c1-16) \
 && mkdir /out/srv && mv /out/engine /out/srv/$v && echo "/engine/$v/" > /out/engine-path

FROM node:24@sha256:14078204b6bb268c107ceff6d3c43f45a888105e5c2b18b14c32b3409d93a1b0 AS webbuild
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --ignore-scripts
COPY web/ ./
COPY --from=engine /out/engine-path /tmp/engine-path
RUN ENGINE_PATH="$(cat /tmp/engine-path)" npm run build

# The assembled server tree: stock + overlay + our config.
FROM hlds AS server
COPY --from=gungame /overlay/ /opt/hlds/
COPY server-config/server.cfg /opt/hlds/cstrike/server.cfg
COPY server-config/metamod-plugins.ini /opt/hlds/cstrike/addons/metamod/plugins.ini
COPY server-config/modes/ /opt/hlds/cstrike/modes/
COPY server-config/reunion.cfg /tmp/reunion.override
COPY --from=assets /out/mapcycle.txt /opt/hlds/cstrike/mapcycle.txt
# community map files next to the stock ones, never replacing one (-n); the catalog (manifest) lists the usable maps
COPY --from=extramaps /extra/ /tmp/extra/
RUN cp -rn /tmp/extra/. /opt/hlds/cstrike/ && rm -rf /tmp/extra
COPY scripts/assemble-server.sh /usr/local/bin/assemble-server
RUN assemble-server /opt/hlds/cstrike /tmp/reunion.override && rm /tmp/reunion.override

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
COPY --from=engine /out/srv/ /srv/engine/
COPY --from=teamart /out/teams/ /srv/img/teams/
COPY --from=assets /out/assets/ /srv/assets/
COPY --from=assets /out/manifest.json /srv/manifest.json
