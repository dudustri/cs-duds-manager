// Typed client for the backend HTTP API. Erasable-only TypeScript: transport.ts imports it under node --test.

/** A room setting: a server cvar and the values it may take ([value, label]). */
export interface Option { cvar: string; label: string; default: string; values: [string, string][] }
export interface Catalog { maps: string[]; modes: string[]; options: Record<string, Option[]>; modeMaps: Record<string, string[]> }
export interface Room {
  id: string; map: string; mode: string; players: number; maxPlayers: number; createdAt: string;
  host?: string; settings?: { label: string; value: string }[];
}
export interface CreatedRoom extends Room { hostToken: string }
export interface Player { userid: number; name: string; steamid: string; frags: number; ping: number; bot: boolean }

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function failure(res: Response): Promise<ApiError> {
  const text = await res.text();
  try {
    const j = JSON.parse(text) as { error?: string; code?: string };
    return new ApiError(res.status, j.code ?? 'error', j.error ?? `HTTP ${res.status}`);
  } catch {
    return new ApiError(res.status, 'error', text || `HTTP ${res.status}`);
  }
}

async function call(method: string, path: string, body?: unknown, auth?: string): Promise<Response> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (auth) headers.Authorization = auth;
  const res = await fetch(`/api${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'same-origin',
  });
  if (!res.ok) throw await failure(res);
  return res;
}

const enc = encodeURIComponent;

export const getCatalog = async (): Promise<Catalog> => (await call('GET', '/catalog')).json();
export const listRooms = async (): Promise<Room[]> => (await call('GET', '/rooms')).json();
/** Everyone online in every room (names and frags only). */
export interface OnlinePlayer { name: string; frags: number; room: string; map: string; mode: string }
export const onlinePlayers = async (): Promise<OnlinePlayer[]> => (await call('GET', '/players')).json();
export const createRoom = async (map: string, mode: string, maxPlayers: number, options: Record<string, string> = {}, host = ''): Promise<CreatedRoom> =>
  (await call('POST', '/rooms', { map, mode, maxPlayers, options, host })).json();
export const deleteRoom = async (id: string): Promise<void> => void (await call('DELETE', `/rooms/${enc(id)}`));

/** Authorization header value: the host's token, a player's session token, or none (admin cookie). */
export type Auth = string | undefined;
export const hostAuth = (token?: string): Auth => (token ? `Bearer ${token}` : undefined);
export const sessionAuth = (token?: string): Auth => (token ? `Session ${token}` : undefined);

export const listPlayers = async (id: string, auth: Auth): Promise<Player[]> =>
  (await call('GET', `/rooms/${enc(id)}/players`, undefined, auth)).json();
export const kick = async (id: string, userid: number, auth: Auth): Promise<void> =>
  void (await call('POST', `/rooms/${enc(id)}/kick`, { userid }, auth));
export const voteMap = async (id: string, maps: string[], auth: Auth): Promise<void> =>
  void (await call('POST', `/rooms/${enc(id)}/votes`, { kind: 'map', maps }, auth));
export const voteKick = async (id: string, userid: number, auth: Auth): Promise<void> =>
  void (await call('POST', `/rooms/${enc(id)}/votes`, { kind: 'kick', userid }, auth));
export const addBot = async (id: string, team: 'ct' | 't', difficulty: number, auth: Auth): Promise<void> =>
  void (await call('POST', `/rooms/${enc(id)}/bots`, { team, difficulty }, auth));
export const stopRoom = async (id: string, auth: Auth): Promise<void> => void (await call('POST', `/rooms/${enc(id)}/stop`, undefined, auth));
export const removeBots = async (id: string, auth: Auth): Promise<void> =>
  void (await call('DELETE', `/rooms/${enc(id)}/bots`, undefined, auth));
export const adminLogin = async (password: string): Promise<void> => void (await call('POST', '/admin/login', { password }));

export async function isAdmin(): Promise<boolean> {
  const res = await fetch('/api/admin', { credentials: 'same-origin' });
  return res.status === 204;
}

/** Posts the browser's SDP offer; returns the relay's SDP answer and this session's vote token. */
export async function openSession(id: string, offerSdp: string, auth?: Auth): Promise<{ answer: string; token: string }> {
  const headers: Record<string, string> = { 'Content-Type': 'application/sdp' };
  if (auth) headers.Authorization = auth;
  const res = await fetch(`/api/rooms/${enc(id)}/session`, { method: 'POST', headers, body: offerSdp });
  if (!res.ok) throw await failure(res);
  return { answer: await res.text(), token: res.headers.get('X-Session-Token') ?? '' };
}

// Host tokens live in localStorage; a private window falls back to memory.
const KEY = 'cs-host-tokens';
const memory: Record<string, string> = {};
function read(): Record<string, string> {
  try {
    return { ...JSON.parse(localStorage.getItem(KEY) ?? '{}'), ...memory };
  } catch {
    return { ...memory };
  }
}
export const hostTokens = {
  get: (roomId: string): string | undefined => read()[roomId],
  set(roomId: string, token: string): void {
    // keep the newest 20 (insertion order); older rooms are long gone, their tokens are dead weight
    const all = Object.entries(read()).filter(([id]) => id !== roomId);
    const kept = Object.fromEntries([...all.slice(-19), [roomId, token]]);
    for (const id of Object.keys(memory)) delete memory[id];
    Object.assign(memory, kept);
    try {
      localStorage.setItem(KEY, JSON.stringify(kept));
    } catch {
      /* memory only */
    }
  },
};
