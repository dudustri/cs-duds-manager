// Page entry: the lobby (server list + create card) and the game (full-viewport container with the canvas, a
// loading screen while joining, and an overlay while the mouse is free). See game.ts for the phase that switches them.
import * as api from './api.ts';
import './controls.ts';
import { bootOnly, state } from './game.ts';
import { loadCatalog, refreshRooms } from './lobby.ts';
import { initName } from './name.ts';

void initName();

void api.isAdmin().then((a) => (state.isAdmin = a), () => {});
void loadCatalog();
void refreshRooms();
setInterval(() => (void loadCatalog(), void refreshRooms()), 5000);
if (new URLSearchParams(location.search).has('boot')) bootOnly();
