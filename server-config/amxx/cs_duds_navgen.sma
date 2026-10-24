/* cs-duds navgen: random spawns (mp_randomspawn, ReGameDLL) pick their points from the map's bot navigation file,
 * which most maps don't ship. zBot builds one when a bot joins a map without it (a minute or two on big maps; the game
 * keeps running). So on such a map, at the first human's spawn, this adds one helper bot to start the build and kicks
 * that same bot once the file is saved. The file is kept (a volume, see assemble-server.sh), so each map is built once.
 */
#include <amxmodx>
#include <hamsandwich>

#define TASK_POLL 7401

new g_nav[64];
new g_helper[32]; // the helper bot's name, once it is in
new bool:g_building, bool:g_waitingBot;

public plugin_init()
{
	register_plugin("cs-duds navgen", "1.0", "cs-duds");
	new map[32];
	get_mapname(map, charsmax(map));
	formatex(g_nav, charsmax(g_nav), "maps/%s.nav", map);
	RegisterHam(Ham_Spawn, "player", "on_spawn", 1);
}

public client_putinserver(id)
{
	if (!g_waitingBot || !is_user_bot(id)) return;
	g_waitingBot = false;
	get_user_name(id, g_helper, charsmax(g_helper));
}

// zBot adds no bot while no human is on a team, so start at the first human's spawn
public on_spawn(id)
{
	if (g_building || !is_user_alive(id) || is_user_bot(id) || !get_cvar_num("mp_randomspawn") || file_exists(g_nav)) return;
	g_building = true;
	g_waitingBot = true;
	server_print("[cs-duds navgen] building %s", g_nav);
	server_cmd("bot_add");
	client_print(0, print_chat, "[cs-duds] Mapping this map for random spawns, a minute or two. Until then you spawn at your team base.");
	set_task(2.0, "poll", TASK_POLL, _, _, "b");
}

// the bot builds the navigation while it plays; once the file is saved it has done its job
public poll()
{
	if (!file_exists(g_nav) || g_helper[0] == 0) return;
	remove_task(TASK_POLL);
	server_print("[cs-duds navgen] %s saved", g_nav);
	server_cmd("bot_kick ^"%s^"", g_helper); // by name: lowers bot_quota again, so it doesn't come back
	client_print(0, print_chat, "[cs-duds] Random spawns are on for this map.");
}
