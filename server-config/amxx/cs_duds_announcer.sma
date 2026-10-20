/* cs-duds announcer: the classic Unreal-style kill sounds (first blood, headshot, double/multi/monster kill,
 * killing spree ... wicked sick, humiliation, last man standing). Sounds live in sound/misc/ and ship in the
 * browser's base bundle, so `spk` plays them from the client's own files.
 */
#include <amxmodx>

#define MULTI_WINDOW 1.5

// kills within MULTI_WINDOW seconds of each other: index = count - 2
new const MULTI[][] = { "doublekill", "multikill", "megakill", "ultrakill", "monsterkill" };
new const MULTI_TEXT[][] = { "Double Kill!", "Multi Kill!", "Mega Kill!", "Ultra Kill!!", "M-M-M-MONSTER KILL!!!" };
// kills in one life: index = count - 3
new const STREAK[][] = { "killingspree", "dominating", "rampage", "unstoppable", "wickedsick", "godlike" };
new const STREAK_TEXT[][] = { "is on a killing spree!", "is dominating!", "is on a rampage!", "is unstoppable!", "is wicked sick!", "is GODLIKE!" };
new const OTHER[][] = { "firstblood", "headshot", "humiliation", "oneandonly", "maytheforce", "prepare" };

new g_streak[33], g_multi[33];
new Float:g_lastKill[33];
new bool:g_firstBlood;
new g_lastMan; // 0 none, 1 "one and only" played, 2 duel played
new g_hud;
new g_enabled; // cs_duds_announcer: a room option (cs_duds_votes.sma applies it)

public plugin_precache()
{
	new i;
	for (i = 0; i < sizeof(MULTI); i++) precache_misc(MULTI[i]);
	for (i = 0; i < sizeof(STREAK); i++) precache_misc(STREAK[i]);
	for (i = 0; i < sizeof(OTHER); i++) precache_misc(OTHER[i]);
}

precache_misc(const name[])
{
	new path[64];
	formatex(path, charsmax(path), "misc/%s.wav", name);
	precache_sound(path);
}

public plugin_init()
{
	register_plugin("cs-duds announcer", "1.0", "cs-duds");
	register_event("DeathMsg", "on_death", "a");
	register_logevent("on_round_start", 2, "1=Round_Start");
	g_hud = CreateHudSyncObj();
	g_enabled = register_cvar("cs_duds_announcer", "1");
}

public client_disconnected(id)
{
	g_streak[id] = 0;
	g_multi[id] = 0;
}

public on_round_start()
{
	g_firstBlood = false;
	g_lastMan = 0;
	play(0, "prepare");
}

announce(const fmt[], any:...)
{
	if (!get_pcvar_num(g_enabled)) return;
	new text[96];
	vformat(text, charsmax(text), fmt, 2);
	set_hudmessage(255, 60, 40, -1.0, 0.25, 0, 0.0, 2.5, 0.1, 0.3, -1);
	ShowSyncHudMsg(0, g_hud, "%s", text);
}

play(id, const name[])
{
	if (!get_pcvar_num(g_enabled)) return;
	client_cmd(id, "spk misc/%s", name);
}

public on_death()
{
	new killer = read_data(1), victim = read_data(2), hs = read_data(3);
	new weapon[16];
	read_data(4, weapon, charsmax(weapon));
	g_streak[victim] = 0;
	g_multi[victim] = 0;
	if (killer < 1 || killer > 32 || killer == victim || !is_user_connected(killer)) return last_man();
	if (get_user_team(killer) == get_user_team(victim)) return last_man(); // team kill: no reward

	new name[32];
	get_user_name(killer, name, charsmax(name));
	new Float:now = get_gametime();
	g_multi[killer] = (now - g_lastKill[killer] <= MULTI_WINDOW) ? g_multi[killer] + 1 : 1;
	g_lastKill[killer] = now;
	g_streak[killer]++;

	// one sound per kill, most special first
	if (!g_firstBlood)
	{
		g_firstBlood = true;
		play(0, "firstblood");
		announce("%s drew first blood!", name);
	}
	else if (g_multi[killer] >= 2)
	{
		new i = min(g_multi[killer] - 2, sizeof(MULTI) - 1);
		play(0, MULTI[i]);
		announce("%s: %s", name, MULTI_TEXT[i]);
	}
	else if (g_streak[killer] >= 3 && g_streak[killer] - 3 < sizeof(STREAK))
	{
		play(0, STREAK[g_streak[killer] - 3]);
		announce("%s %s", name, STREAK_TEXT[g_streak[killer] - 3]);
	}
	else if (equal(weapon, "knife"))
	{
		play(0, "humiliation");
		announce("%s humiliated a victim with the knife!", name);
	}
	else if (hs)
	{
		play(killer, "headshot");
	}
	return last_man();
}

// the moment a team is down to one player: "one and only" against many, "may the force" in a 1 on 1
last_man()
{
	if (g_lastMan == 2 || get_cvar_num("mp_forcerespawn")) return PLUGIN_CONTINUE; // once per round; meaningless with respawns
	new t[32], ct[32], nt, nct;
	get_players(t, nt, "ae", "TERRORIST");
	get_players(ct, nct, "ae", "CT");
	if (nt == 1 && nct == 1)
	{
		play(0, "maytheforce");
		g_lastMan = 2;
	}
	else if (g_lastMan == 0 && (nt == 1) != (nct == 1) && nt + nct > 2)
	{
		play(nt == 1 ? t[0] : ct[0], "oneandonly");
		g_lastMan = 1;
	}
	return PLUGIN_CONTINUE;
}
