/* cs-duds chat votes: "rtv" and "/votekick <name>" for every player.
 * Both only *start* the stock AMXX votes (adminvote.amxx) from the server console, so the whole room votes
 * and amx_vote_ratio / amx_vote_delay apply. Chat text never reaches a server command: maps come from the
 * mapcycle (generated from the catalog) and kicks are passed as a numeric #userid.
 */
#include <amxmodx>
#include <amxmisc>

#define MAX_MAPS 64
#define ASK_COOLDOWN 30.0

new g_maps[MAX_MAPS][32];
new g_mapCount;
new bool:g_rtv[33];
new Float:g_lastAsk[33];

public plugin_init()
{
	register_plugin("cs-duds votes", "1.0", "cs-duds");
	register_clcmd("say rtv", "cmd_rtv");
	register_clcmd("say /rtv", "cmd_rtv");
	register_clcmd("say_team rtv", "cmd_rtv");
	register_clcmd("say", "cmd_say");
	register_clcmd("say_team", "cmd_say");
}

public plugin_cfg()
{
	new file[64], line[64], len;
	get_cvar_string("mapcyclefile", file, charsmax(file));
	for (new i = 0; g_mapCount < MAX_MAPS && read_file(file, i, line, charsmax(line), len); i++)
	{
		trim(line);
		if (line[0] && line[0] != ';' && line[0] != '/' && is_map_valid(line))
			copy(g_maps[g_mapCount++], charsmax(g_maps[]), line);
	}
}

public client_disconnected(id)
{
	g_rtv[id] = false;
}

bool:too_soon(id)
{
	new Float:now = get_gametime();
	if (g_lastAsk[id] > 0.0 && now - g_lastAsk[id] < ASK_COOLDOWN)
	{
		client_print(id, print_chat, "[cs-duds] Wait a bit before asking again.");
		return true;
	}
	g_lastAsk[id] = now;
	return false;
}

humans()
{
	new players[32], num;
	get_players(players, num, "ch");
	return num;
}

public cmd_rtv(id)
{
	if (g_rtv[id])
	{
		client_print(id, print_chat, "[cs-duds] You already rocked the vote.");
		return PLUGIN_CONTINUE;
	}
	g_rtv[id] = true;
	new players[32], num, count;
	get_players(players, num, "ch");
	for (new i = 0; i < num; i++)
		if (g_rtv[players[i]]) count++;
	new need = humans() / 2 + 1;
	if (count < need)
	{
		new name[32];
		get_user_name(id, name, charsmax(name));
		client_print(0, print_chat, "[cs-duds] %s wants a map vote (%d/%d). Say rtv to agree.", name, count, need);
		return PLUGIN_CONTINUE;
	}
	start_map_vote();
	return PLUGIN_CONTINUE;
}

start_map_vote()
{
	new current[32], cmd[160], picked[4], n;
	get_mapname(current, charsmax(current));
	// pick up to 4 distinct catalog maps other than the current one, in random order
	for (new tries = 0; n < 4 && tries < 64 && g_mapCount > 0; tries++)
	{
		new m = random(g_mapCount), dup = false;
		if (equali(g_maps[m], current)) continue;
		for (new k = 0; k < n; k++) if (picked[k] == m) dup = true;
		if (!dup) picked[n++] = m;
	}
	if (n == 0) return;
	copy(cmd, charsmax(cmd), "amx_votemap");
	for (new k = 0; k < n; k++) format(cmd, charsmax(cmd), "%s %s", cmd, g_maps[picked[k]]);
	server_cmd("%s", cmd);
	arrayset(g_rtv, false, sizeof(g_rtv));
}

public cmd_say(id)
{
	new args[96];
	read_args(args, charsmax(args));
	remove_quotes(args);
	trim(args);
	if (!equali(args, "/votekick", 9)) return PLUGIN_CONTINUE;
	new target_name[64];
	copy(target_name, charsmax(target_name), args[9]);
	trim(target_name);
	if (!target_name[0])
	{
		client_print(id, print_chat, "[cs-duds] Usage: /votekick <part of a name>");
		return PLUGIN_HANDLED;
	}
	if (too_soon(id)) return PLUGIN_HANDLED;
	new target = cmd_target(id, target_name, CMDTARGET_NO_BOTS);
	if (!target) return PLUGIN_HANDLED; // cmd_target already told the player why
	server_cmd("amx_votekick #%d", get_user_userid(target));
	return PLUGIN_CONTINUE;
}
