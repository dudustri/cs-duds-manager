/* cs-duds zombie infection: 15 seconds into each round one random player turns into a zombie (Terrorist, knife
 * only, lots of health, faster). A zombie's hit turns a human (Counter-Terrorist) into a zombie. Zombies win when
 * no human is left; humans win if they last until the round timer runs out. Everyone starts each round human.
 */
#include <amxmodx>
#include <cstrike>
#include <fun>
#include <hamsandwich>

#define ZOMBIE_HP 2000
#define ZOMBIE_SPEED 300.0
#define FIRST_DELAY 15.0
#define TASK_FIRST 7301

new bool:g_zombie[33];
new bool:g_infecting; // the first zombie is out: hits infect, the last human ends the round

public plugin_init()
{
	register_plugin("cs-duds zombie", "1.0", "cs-duds");
	register_event("HLTV", "on_new_round", "a", "1=0", "2=0");
	register_logevent("on_round_start", 2, "1=Round_Start");
	register_event("CurWeapon", "on_weapon", "be", "1=1");
	RegisterHam(Ham_TakeDamage, "player", "on_damage");
	RegisterHam(Ham_Spawn, "player", "on_spawn", 1);
}

public client_disconnected(id)
{
	g_zombie[id] = false;
	check_humans();
}

// before anyone spawns: everyone back to human
public on_new_round()
{
	remove_task(TASK_FIRST);
	g_infecting = false;
	new players[32], num;
	get_players(players, num);
	for (new i = 0; i < num; i++)
	{
		new id = players[i];
		g_zombie[id] = false;
		new CsTeams:team = cs_get_user_team(id);
		if (team == CS_TEAM_T) cs_set_user_team(id, CS_TEAM_CT);
	}
}

public on_round_start()
{
	client_print(0, print_center, "The infection starts in 15 seconds");
	set_task(FIRST_DELAY, "pick_first", TASK_FIRST);
}

public pick_first()
{
	new players[32], num;
	get_players(players, num, "a");
	if (num < 2) return client_print(0, print_chat, "[cs-duds] Zombie needs at least 2 players alive.");
	new id = players[random(num)];
	new name[32];
	get_user_name(id, name, charsmax(name));
	infect(id);
	g_infecting = true;
	set_hudmessage(224, 86, 63, -1.0, 0.3, 0, 0.0, 4.0, 0.1, 0.5, -1);
	show_hudmessage(0, "%s is the first zombie. Run!", name);
	return PLUGIN_HANDLED;
}

infect(id)
{
	g_zombie[id] = true;
	cs_set_user_team(id, CS_TEAM_T);
	strip_user_weapons(id);
	give_item(id, "weapon_knife");
	set_user_health(id, ZOMBIE_HP);
	set_user_maxspeed(id, ZOMBIE_SPEED);
}

public on_spawn(id)
{
	if (is_user_alive(id) && g_zombie[id]) infect(id); // a respawned zombie stays one this round
}

// a zombie's hit infects instead of hurting; zombies keep the knife and their speed
public on_damage(victim, inflictor, attacker, Float:damage, bits)
{
	if (!g_infecting || attacker < 1 || attacker > 32 || attacker == victim) return HAM_IGNORED;
	if (g_zombie[attacker] && !g_zombie[victim])
	{
		new a[32], v[32];
		get_user_name(attacker, a, charsmax(a));
		get_user_name(victim, v, charsmax(v));
		infect(victim);
		client_print(0, print_chat, "[cs-duds] %s infected %s", a, v);
		check_humans();
		return HAM_SUPERCEDE;
	}
	return HAM_IGNORED;
}

public on_weapon(id)
{
	if (!g_zombie[id]) return;
	if (read_data(2) != CSW_KNIFE)
	{
		strip_user_weapons(id);
		give_item(id, "weapon_knife");
	}
	set_user_maxspeed(id, ZOMBIE_SPEED);
}

check_humans()
{
	if (!g_infecting) return;
	new players[32], num;
	get_players(players, num, "ae", "CT");
	for (new i = 0; i < num; i++) if (!g_zombie[players[i]]) return;
	g_infecting = false;
	set_hudmessage(224, 86, 63, -1.0, 0.3, 0, 0.0, 5.0, 0.1, 0.5, -1);
	show_hudmessage(0, "Zombies win!");
	server_cmd("sv_restartround 5");
}
