// Small DOM helpers shared by the page modules.
import * as api from './api.ts';

export const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;

export const errorText = (e: unknown): string => {
  if (e instanceof api.ApiError) {
    const known: Record<string, string> = {
      room_not_found: 'That room no longer exists.',
      too_many_sessions: 'Too many connections from your network. Try again in a minute.',
      rate_limited: 'Too many requests. Wait a moment and try again.',
      too_many_rooms: 'You already have the maximum number of rooms running. Join one of them or wait for it to close.',
      rooms_full: 'All room slots are taken. Join a room or wait for one to close.',
      start_failed: 'The room did not start in time. Try again.',
      vote_running: 'A vote is already running.',
      vote_too_soon: 'A vote just ended. Wait a few seconds before starting another.',
      unknown_map: 'The server does not know that map.',
      name_taken: 'Someone already plays under that name. Pick another one.',
      bad_name: 'Names are 1-31 characters, without quotes, semicolons or backslashes.',
    };
    return known[e.code] ?? e.message;
  }
  return e instanceof Error ? e.message : String(e);
};

/** Sets the text of a note/status element; `bad` styles it as an error. */
export function setNote(id: string, text: string, bad = false): void {
  const el = $(id);
  el.textContent = text;
  el.classList.toggle('bad', bad);
}
