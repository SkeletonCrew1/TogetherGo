import { api } from './client'
import type { ChatMessage, ChatRoom, ChatTicket, Page } from '@/lib/types'

/**
 * `/api/chat/*` — the chat service's REST surface, which is read-only.
 *
 * There is deliberately no `send` here. The service exposes no write path for a
 * message: `POST /api/chat/rooms/{tripId}/messages` does not exist and answers
 * 405, because a room is a WebSocket and a client that has one open sends over
 * it (CLAUDE.md: "WebSockets only for chat"). The composer's send lives in
 * `@/lib/ws/chatSocket`.
 */

/** Rooms the caller is a member of, newest activity first. */
export const rooms = (cursor?: string | null) =>
  api.get<Page<ChatRoom>>('/api/chat/rooms', { query: { cursor: cursor ?? undefined } })

/**
 * History, newest first. Paging backwards through time is what a thread does:
 * the cursor walks towards older messages, and the live socket delivers newer
 * ones.
 */
export const messages = (tripId: string, cursor?: string | null) =>
  api.get<Page<ChatMessage>>(`/api/chat/rooms/${tripId}/messages`, {
    query: { cursor: cursor ?? undefined },
  })

/**
 * A single-use, short-lived ticket for the WebSocket handshake.
 *
 * The browser WebSocket API cannot set an Authorization header, and putting a
 * 15-minute access token in a query string writes it to every proxy log between
 * here and the service. A ticket is requested with the token, spent once on
 * connect, and re-requested on every reconnect.
 *
 * `signal` is not optional decoration. A ticket is single-use, so a request
 * whose answer nobody will spend has burned one — which is exactly what a
 * connect attempt abandoned mid-flight does, and what StrictMode's double
 * mount used to do on every page load. The caller aborts instead.
 */
export const ticket = (tripId: string, signal?: AbortSignal) =>
  api.post<ChatTicket>('/api/chat/tickets', { trip_id: tripId }, { signal })
