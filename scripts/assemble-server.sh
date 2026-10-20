#!/bin/sh
# scripts/assemble-server.sh CSTRIKE_DIR REUNION_OVERRIDE
# Patches the merged server tree (stock HLDS + addons overlay + server-config) in place. The Dockerfile runs it on
# the real tree at build; scripts/assemble-server.test.sh runs it on a fixture tree in the fast tier.
set -eu
[ $# -eq 2 ] || { echo "usage: assemble-server.sh CSTRIKE_DIR REUNION_OVERRIDE" >&2; exit 2; }
cd "$1"
override=$2
fail() { echo "assemble-server: $*" >&2; exit 1; }

# metamod becomes the game DLL; it finds the real one (ReGameDLL's dlls/cs.so) by itself
sed -i 's#^gamedll_linux .*#gamedll_linux "addons/metamod/metamod_i386.so"#' liblist.gam
grep -q '^gamedll_linux "addons/metamod/metamod_i386.so"' liblist.gam || fail "liblist.gam has no gamedll_linux"

# ReUnion: the stock file with our keys replaced in place; the salt is random per build
salt=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
tmp=$(mktemp)
sed "s/@SALT@/$salt/" "$override" | awk -F'[ \t]*=[ \t]*' 'NR==FNR { if ($0 !~ /^#/ && NF >= 2) kv[$1] = $2; next }
     ($1 in kv) { print $1 " = " kv[$1]; seen[$1] = 1; next } { print }
     END { for (k in kv) if (!(k in seen)) { print "missing key " k > "/dev/stderr"; exit 1 } }' - reunion.cfg > "$tmp"
mv "$tmp" reunion.cfg
grep -q "^SteamIdHashSalt = $salt\$" reunion.cfg || fail "reunion.cfg salt not applied"

# core.ini's amxx_plugins / amxx_configsdir would override the per-room `+localinfo` values (the backend gives each
# room its own plugin list and configs dir); drop them, AMXX defaults to the same paths
sed -i '/^amxx_plugins[[:space:]]/d;/^amxx_configsdir[[:space:]]/d' addons/amxmodx/configs/core.ini
! grep -q '^amxx_\(plugins\|configsdir\)[[:space:]]' addons/amxmodx/configs/core.ini || fail "core.ini still sets amxx paths"

# zBot on, no automatic bots: the host adds them from the overlay (BotProfile.db comes from the overlay)
sed -i 's/^bot_enable .*/bot_enable "1"/' game_init.cfg
printf '\nbot_quota "0"\nbot_join_after_player "0"\nbot_chatter "off"\n' >> game_init.cfg
grep -q '^bot_enable "1"' game_init.cfg || fail "game_init.cfg has no bot_enable"

# the map votes (mapsmenu, mapchooser, rtv) offer the catalog only
cp mapcycle.txt addons/amxmodx/configs/maps.ini

# per-mode AMXX plugin list = stock plugins.ini (admin.amxx + adminvote.amxx included) + ours + the mode's extras
mkdir -p addons/amxmodx/configs/modes
for d in modes/*/; do
  m=$(basename "$d")
  ini=addons/amxmodx/configs/modes/$m.ini
  { cat addons/amxmodx/configs/plugins.ini; printf '\ncs_duds_votes.amxx\ncs_duds_announcer.amxx\n'; [ ! -f "$d/plugins.txt" ] || cat "$d/plugins.txt"; } > "$ini"
  extras=$( [ ! -f "$d/plugins.txt" ] || awk '$1 !~ /^;/ && NF { print $1 }' "$d/plugins.txt")
  for p in admin.amxx adminvote.amxx cs_duds_votes.amxx cs_duds_announcer.amxx $extras; do
    grep -q "^$p" "$ini" || fail "$ini lacks $p"
  done
done

rm -rf addons/amxmodx/scripting
