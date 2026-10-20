/* cs-duds climb timer (kz mode): times each player from the map's start button to its stop button and keeps
 * the best time of the session. KZ maps name those buttons by target or targetname: "counter_start" /
 * "counter_off", or anything with "start" / "stop", "end" or "finish" in it. Players can't hurt each other.
 */
#include <amxmodx>
#include <fakemeta>
#include <hamsandwich>

new Float:g_start[33], Float:g_best[33];
new bool:g_running[33];
new g_hud;

public plugin_init()
{
	register_plugin("cs-duds timer", "1.0", "cs-duds");
	RegisterHam(Ham_Use, "func_button", "on_button");
	RegisterHam(Ham_TakeDamage, "player", "on_damage");
	register_clcmd("say /reset", "cmd_reset");
	g_hud = CreateHudSyncObj();
	set_task(0.1, "show_timers", _, _, _, "b");
}

public client_disconnected(id)
{
	g_running[id] = false;
	g_best[id] = 0.0;
}

// which kind of button: 1 start, 2 stop, 0 neither
button_kind(ent)
{
	new names[2][32];
	pev(ent, pev_target, names[0], charsmax(names[]));
	pev(ent, pev_targetname, names[1], charsmax(names[]));
	for (new i = 0; i < 2; i++)
	{
		if (equali(names[i], "counter_start") || containi(names[i], "start") != -1) return 1;
		if (equali(names[i], "counter_off") || containi(names[i], "stop") != -1 || containi(names[i], "end") != -1
			|| containi(names[i], "finish") != -1) return 2;
	}
	return 0;
}

public on_button(ent, id)
{
	if (id < 1 || id > 32 || !is_user_alive(id)) return HAM_IGNORED;
	switch (button_kind(ent))
	{
		case 1:
		{
			g_start[id] = get_gametime();
			g_running[id] = true;
			client_print(id, print_center, "Timer started");
		}
		case 2:
		{
			if (!g_running[id]) return HAM_IGNORED;
			g_running[id] = false;
			new Float:t = get_gametime() - g_start[id];
			new name[32], text[16];
			get_user_name(id, name, charsmax(name));
			format_time_ms(t, text, charsmax(text));
			new bool:best = g_best[id] == 0.0 || t < g_best[id];
			if (best) g_best[id] = t;
			client_print(0, print_chat, "[cs-duds] %s finished in %s%s", name, text, best ? " (personal best)" : "");
		}
	}
	return HAM_IGNORED;
}

public on_damage(victim, inflictor, attacker, Float:damage, bits)
{
	return (attacker >= 1 && attacker <= 32 && attacker != victim) ? HAM_SUPERCEDE : HAM_IGNORED;
}

public cmd_reset(id)
{
	g_running[id] = false;
	client_print(id, print_chat, "[cs-duds] Timer stopped. Press the start button to run again.");
	return PLUGIN_HANDLED;
}

format_time_ms(Float:t, out[], len)
{
	new ms = floatround(t * 100.0, floatround_floor);
	formatex(out, len, "%02d:%02d.%02d", ms / 6000, (ms / 100) % 60, ms % 100);
}

public show_timers()
{
	new players[32], num, text[16];
	get_players(players, num, "ach");
	set_hudmessage(255, 171, 46, -1.0, 0.9, 0, 0.0, 0.2, 0.0, 0.0, -1);
	for (new i = 0; i < num; i++)
	{
		new id = players[i];
		if (!g_running[id]) continue;
		format_time_ms(get_gametime() - g_start[id], text, charsmax(text));
		ShowSyncHudMsg(id, g_hud, "%s", text);
	}
}
