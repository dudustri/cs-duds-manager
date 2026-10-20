#!/bin/sh
# Runs scripts/assemble-server.sh on a minimal fixture tree with the real server-config and checks the result.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fail() { echo "assemble-server.test: $*" >&2; exit 1; }

mkdir -p "$t/addons/amxmodx/configs" "$t/addons/amxmodx/scripting"
printf 'game "Counter-Strike"\ngamedll_linux "dlls/cs.so"\n' > "$t/liblist.gam"
# the stock keys our override replaces
awk -F'[ \t]*=[ \t]*' '$0 !~ /^#/ && NF >= 2 { print $1 " = 1" }' "$root/server-config/reunion.cfg" > "$t/reunion.cfg"
printf 'amxx_logs addons/amxmodx/logs\namxx_pluginsdir addons/amxmodx/plugins\namxx_plugins addons/amxmodx/configs/plugins.ini\namxx_configsdir x\n' > "$t/addons/amxmodx/configs/core.ini"
printf 'bot_enable "0"\n' > "$t/game_init.cfg"
printf 'admin.amxx\nadminvote.amxx\n' > "$t/addons/amxmodx/configs/plugins.ini"
printf 'de_dust2\n' > "$t/mapcycle.txt"
cp -r "$root/server-config/modes" "$t/modes"

"$root/scripts/assemble-server.sh" "$t" "$root/server-config/reunion.cfg"

grep -q '^gamedll_linux "addons/metamod/metamod_i386.so"' "$t/liblist.gam" || fail "liblist.gam"
grep -q '^SteamIdHashSalt = [0-9a-f]\{48\}$' "$t/reunion.cfg" || fail "reunion salt"
grep -q '^cid_RevEmu2013 = 3$' "$t/reunion.cfg" || fail "reunion override"
! grep -q '^amxx_plugins[[:space:]]' "$t/addons/amxmodx/configs/core.ini" || fail "core.ini"
grep -q '^bot_enable "1"' "$t/game_init.cfg" || fail "game_init.cfg"
[ -f "$t/addons/amxmodx/configs/maps.ini" ] || fail "maps.ini"
for m in classic deathmatch gungame; do
  grep -q '^cs_duds_announcer.amxx' "$t/addons/amxmodx/configs/modes/$m.ini" || fail "$m.ini"
done
grep -q '^gungame.amxx' "$t/addons/amxmodx/configs/modes/gungame.ini" || fail "gungame.ini"
! grep -q '^gungame.amxx' "$t/addons/amxmodx/configs/modes/classic.ini" || fail "classic.ini has gungame"
[ ! -d "$t/addons/amxmodx/scripting" ] || fail "scripting dir left"
echo "assemble-server.test: ok"
