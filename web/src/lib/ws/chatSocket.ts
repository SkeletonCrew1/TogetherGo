import * as chatApi from '@/lib/api/chat'
import type { ChatFrame, OutboundMessageFrame } from '@/lib/types'

/**
 * The chat WebSocket, as a plain object with a lifecycle — not a hook, so that
 * React's mount/unmount churn (StrictMode double-invokes effects in dev) does
 * not turn into a reconnect storm. useChatRoom owns exactly one of these.
 *
 * The contract it implements:
 *
 *   - request a ticket, then connect — one attempt, one ticket, and the ticket
 *     request is *inside* the attempt so that abandoning the attempt abandons
 *     the request too
 *   - reconnect with exponential backoff on drop
 *   - re-request a fresh ticket on every reconnect, because tickets are
 *     single-use and replaying the last one is a guaranteed 401
 *   - carry the composer's frames: this is the only way a message is sent
 */

export type ConnectionState = 'connecting' | 'open' | 'reconnecting' | 'closed'

export interface ChatSocketHandlers {
  onFrame: (frame: ChatFrame) => void
  onState: (state: ConnectionState) => void
}

/** Backoff: 1s, 2s, 4s, 8s, 16s, then 30s forever, each jittered. */
const BASE_DELAY_MS = 1_000
const MAX_DELAY_MS = 30_000

/**
 * `WebSocket.OPEN`, as a number.
 *
 * Read from the constant rather than off the global so that the check still
 * means something under a test double that does not carry the static fields.
 * The value is fixed by the WHATWG spec and cannot drift.
 */
const SOCKET_OPEN = 1

function backoffDelay(attempt: number): number {
  const exponential = Math.min(BASE_DELAY_MS * 2 ** attempt, MAX_DELAY_MS)
  // Full jitter. Without it, everyone in a room reconnects in lockstep after a
  // service restart and the first thing the service back up sees is the same
  // thundering herd that may have taken it down.
  return Math.random() * exponential
}

/**
 * An explicit websocket origin, for a build served from somewhere other than
 * the gateway. Empty in every deployment this project has: the SPA is served
 * by Traefik on the same origin as `/ws/chat`, so the socket derives its host
 * from the page and nothing has to be configured.
 */
const WS_BASE = import.meta.env.VITE_WS_BASE_URL ?? ''

/**
 * The page's own origin, as a websocket scheme.
 *
 * `window.location.host` and not a baked-in hostname: the page is served
 * through the gateway, so whatever host the browser used to reach it is the
 * host that also routes /ws/chat. The scheme has to be translated because
 * `new WebSocket('http://...')` is a SyntaxError — and it is read from
 * `location.protocol` rather than assumed, so a page served over TLS opens a
 * `wss:` socket instead of a mixed-content one the browser would block.
 */
function sameOriginWsBase(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${window.location.host}`
}

function socketUrl(tripId: string, ticket: string): string {
  const url = new URL(`/ws/chat/${tripId}`, WS_BASE || sameOriginWsBase())
  // A configured VITE_WS_BASE_URL is just as likely to be written with an http
  // scheme as a ws one; normalise rather than hand the constructor something
  // it will throw on.
  if (url.protocol === 'http:') url.protocol = 'ws:'
  else if (url.protocol === 'https:') url.protocol = 'wss:'
  url.searchParams.set('ticket', ticket)
  return url.toString()
}

export class ChatSocket {
  private socket: WebSocket | null = null
  private attempt = 0
  /** The pending connect or the pending backoff — never both at once. */
  private timer: ReturnType<typeof setTimeout> | null = null
  /** Aborts the ticket request of the attempt currently in flight. */
  private ticketAbort: AbortController | null = null
  private stopped = true

  constructor(
    private readonly tripId: string,
    private readonly handlers: ChatSocketHandlers,
  ) {}

  /**
   * Begins connecting, on the next tick rather than right now.
   *
   * That one-tick delay is what makes StrictMode's double mount cost one ticket
   * instead of two. React mounts the effect, unmounts it and mounts it again
   * synchronously within a single commit, so a connect started here would
   * already have a `fetch` in the air by the time the cleanup ran. Deferred, the
   * discarded mount's attempt is cancelled by `stop()` before it ever asks for a
   * ticket — and `stop()` aborts the request as well, for the case where the
   * teardown is a real navigation and the attempt is genuinely in flight.
   */
  start(): void {
    this.stopped = false
    this.timer = setTimeout(() => {
      this.timer = null
      void this.connect()
    }, 0)
  }

  /**
   * Tears the whole attempt down: the pending connect, the ticket request in
   * flight, the socket, and the backoff timer. Nothing survives it, so a
   * `start()` after a `stop()` begins a clean cycle rather than racing the last
   * one's continuations.
   */
  stop(): void {
    this.stopped = true
    this.attempt = 0

    if (this.timer) {
      clearTimeout(this.timer)
      this.timer = null
    }
    if (this.ticketAbort) {
      this.ticketAbort.abort()
      this.ticketAbort = null
    }
    if (this.socket) {
      // Drop the handlers first: close() fires onclose, and a reconnect
      // scheduled by our own teardown is the classic version of this bug.
      this.socket.onclose = null
      this.socket.onerror = null
      this.socket.onmessage = null
      this.socket.onopen = null
      this.socket.close(1000, 'client closed')
      this.socket = null
    }
    this.handlers.onState('closed')
  }

  /** Whether a frame written now would actually leave the browser. */
  get isOpen(): boolean {
    return this.socket !== null && this.socket.readyState === SOCKET_OPEN
  }

  /**
   * Writes one frame, and says whether it went.
   *
   * `false` is not an error, it is the offline case: the caller holds the
   * message and hands it back when the connection reopens. There is no HTTP
   * fallback to take, because the service has no endpoint that would accept it.
   */
  send(frame: OutboundMessageFrame): boolean {
    if (!this.isOpen || !this.socket) return false
    try {
      this.socket.send(JSON.stringify(frame))
      return true
    } catch {
      // readyState lied — the socket moved to CLOSING between the check and
      // the write. Treated as "not sent", which is exactly what happened.
      return false
    }
  }

  private async connect(): Promise<void> {
    if (this.stopped) return
    this.handlers.onState(this.attempt === 0 ? 'connecting' : 'reconnecting')

    // One controller per attempt, so that a teardown cancels this attempt's
    // request and cannot cancel the next one's.
    const abort = new AbortController()
    this.ticketAbort = abort

    let ticket: string
    try {
      // Fresh every time. This is the whole reason connect() is async.
      ticket = (await chatApi.ticket(this.tripId, abort.signal)).ticket
    } catch {
      // An aborted request is this attempt being abandoned, and there is
      // nothing to retry: whoever aborted it has already decided.
      if (this.stopped || abort.signal.aborted) return
      // Otherwise: the access token expired and the API client's refresh did
      // not save it, or the service is down. Either way, back off and ask again.
      this.scheduleReconnect()
      return
    } finally {
      if (this.ticketAbort === abort) this.ticketAbort = null
    }

    if (this.stopped || abort.signal.aborted) return

    const socket = new WebSocket(socketUrl(this.tripId, ticket))
    this.socket = socket

    socket.onopen = () => {
      // Reset only once the connection is actually established. Resetting when
      // the attempt *starts* makes the backoff useless against a service that
      // accepts a socket and immediately drops it.
      this.attempt = 0
      this.handlers.onState('open')
    }

    socket.onmessage = (event) => {
      const frame = parseFrame(event.data)
      if (frame) this.handlers.onFrame(frame)
    }

    socket.onerror = () => {
      // onerror is always followed by onclose; reconnecting is handled there so
      // that one drop does not schedule two attempts.
    }

    socket.onclose = () => {
      this.socket = null
      if (this.stopped) return
      this.scheduleReconnect()
    }
  }

  private scheduleReconnect(): void {
    if (this.stopped) return
    const delay = backoffDelay(this.attempt)
    this.attempt += 1
    this.handlers.onState('reconnecting')
    this.timer = setTimeout(() => {
      this.timer = null
      void this.connect()
    }, delay)
  }
}

/**
 * A frame the client understands, or null.
 *
 * Unknown `type` values are dropped rather than thrown on, for the same reason
 * consumers on the bus log-and-ack an unknown event type: a frame this build
 * does not recognise is a message meant for a newer one.
 */
const KNOWN_FRAMES = new Set(['message', 'presence', 'typing', 'error'])

function parseFrame(data: unknown): ChatFrame | null {
  if (typeof data !== 'string') return null
  let parsed: unknown
  try {
    parsed = JSON.parse(data)
  } catch {
    return null
  }
  if (typeof parsed !== 'object' || parsed === null) return null
  const type = (parsed as { type?: unknown }).type
  if (typeof type !== 'string' || !KNOWN_FRAMES.has(type)) return null
  return parsed as ChatFrame
}

/**
 * The id that lets an optimistic row survive its own echo.
 *
 * crypto.randomUUID needs a secure context; localhost counts, but a build
 * served over plain http from anything else does not, hence the fallback.
 */
export function newClientMsgId(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID()
  return `cmid-${Date.now()}-${Math.random().toString(16).slice(2)}`
}
