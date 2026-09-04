import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ChatSocket, type ConnectionState } from './chatSocket'
import * as chatApi from '@/lib/api/chat'

/**
 * The socket's rules: request a ticket then connect, reconnect with exponential
 * backoff on drop, re-request a fresh ticket on every reconnect, and carry the
 * composer's frames.
 *
 * The ones worth a test are the ones about tickets. They are single-use, so a
 * reconnect that replays the last one is a guaranteed 401 and a room that never
 * comes back, and an attempt abandoned between the request and the handshake
 * has spent one for nothing — which is what a StrictMode double mount used to
 * do on every page load.
 */

class FakeWebSocket {
  static instances: FakeWebSocket[] = []
  static readonly OPEN = 1
  static readonly CLOSED = 3

  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  readonly url: string
  readyState = 0
  closed = false
  sent: string[] = []

  constructor(url: string) {
    this.url = url
    FakeWebSocket.instances.push(this)
  }

  send(data: string) {
    this.sent.push(data)
  }

  close() {
    this.closed = true
    this.readyState = FakeWebSocket.CLOSED
  }

  /** Test helpers. */
  open() {
    this.readyState = FakeWebSocket.OPEN
    this.onopen?.()
  }
  drop() {
    this.readyState = FakeWebSocket.CLOSED
    this.onclose?.()
  }
  deliver(data: unknown) {
    this.onmessage?.({ data })
  }
}

let ticketCount = 0
const states: ConnectionState[] = []
const frames: unknown[] = []

beforeEach(() => {
  ticketCount = 0
  states.length = 0
  frames.length = 0
  FakeWebSocket.instances.length = 0

  vi.useFakeTimers()
  vi.stubGlobal('WebSocket', FakeWebSocket)
  // A stable origin: socketUrl falls back to window.location.origin when no
  // API base is configured, which is the dev-proxy setup.
  vi.spyOn(chatApi, 'ticket').mockImplementation(async () => {
    ticketCount += 1
    return { ticket: `tkt-${ticketCount}`, expires_in: 30 }
  })
})

afterEach(() => {
  vi.useRealTimers()
})

function start(tripId = 'trip-1') {
  const socket = new ChatSocket(tripId, {
    onState: (state) => states.push(state),
    onFrame: (frame) => frames.push(frame),
  })
  socket.start()
  return socket
}

describe('ChatSocket', () => {
  it('requests a ticket before connecting and puts it in the URL', async () => {
    const socket = start()
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))

    expect(ticketCount).toBe(1)
    expect(FakeWebSocket.instances[0].url).toContain('/ws/chat/trip-1')
    expect(FakeWebSocket.instances[0].url).toContain('ticket=tkt-1')
    socket.stop()
  })

  /**
   * The StrictMode case, at the level it is actually fixed.
   *
   * React mounts an effect, unmounts it and mounts it again within one commit.
   * A socket that asked for its ticket the moment `start()` was called would
   * have burned one on the mount that React threw away — two POSTs in the same
   * millisecond, one socket, and a single-use credential spent for nothing.
   */
  it('an attempt started and stopped in the same tick never asks for a ticket', async () => {
    const socket = start()
    socket.stop()

    await vi.advanceTimersByTimeAsync(1_000)

    expect(ticketCount).toBe(0)
    expect(FakeWebSocket.instances).toHaveLength(0)
  })

  it('aborts a ticket request that is already in flight when it is stopped', async () => {
    // A ticket request that never settles on its own: the only thing that can
    // end it is the abort.
    let signal: AbortSignal | undefined
    vi.mocked(chatApi.ticket).mockImplementation(
      (_tripId, abortSignal) =>
        new Promise((_resolve, reject) => {
          signal = abortSignal
          abortSignal?.addEventListener('abort', () => reject(new Error('aborted')))
        }),
    )

    const socket = start()
    await vi.waitFor(() => expect(signal).toBeDefined())
    expect(signal?.aborted).toBe(false)

    socket.stop()
    expect(signal?.aborted).toBe(true)

    // And the rejection it causes is not mistaken for a failure worth retrying.
    await vi.advanceTimersByTimeAsync(60_000)
    expect(FakeWebSocket.instances).toHaveLength(0)
  })

  it('requests a fresh ticket on every reconnect', async () => {
    const socket = start()
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))

    FakeWebSocket.instances[0].open()
    FakeWebSocket.instances[0].drop()

    // The reconnect is behind a backoff timer.
    await vi.advanceTimersByTimeAsync(2_000)
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2))

    expect(ticketCount).toBe(2)
    // A replayed ticket is the bug this asserts against.
    expect(FakeWebSocket.instances[1].url).toContain('ticket=tkt-2')
    socket.stop()
  })

  it('backs off further on each successive failure', async () => {
    const socket = start()
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))

    // Drop without ever opening, three times. The delay is jittered, so this
    // asserts the ceiling grows rather than an exact schedule.
    FakeWebSocket.instances[0].drop()
    await vi.advanceTimersByTimeAsync(1_000)
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2))

    FakeWebSocket.instances[1].drop()
    // Under 2s cannot be guaranteed to have fired at attempt 2, but 4s covers
    // the whole jitter range for it.
    await vi.advanceTimersByTimeAsync(4_000)
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(3))

    expect(ticketCount).toBe(3)
    socket.stop()
  })

  it('resets the backoff only once a connection actually opens', async () => {
    const socket = start()
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))

    FakeWebSocket.instances[0].open()
    expect(states).toContain('open')

    FakeWebSocket.instances[0].drop()
    // Attempt counter is back to 0, so the first retry is within the 1s band.
    await vi.advanceTimersByTimeAsync(1_000)
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2))
    socket.stop()
  })

  it('stops for good: no reconnect after stop(), even mid-backoff', async () => {
    const socket = start()
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))

    FakeWebSocket.instances[0].drop()
    socket.stop()

    await vi.advanceTimersByTimeAsync(60_000)
    expect(FakeWebSocket.instances).toHaveLength(1)
    expect(states.at(-1)).toBe('closed')
  })

  it('retries when the ticket request itself fails', async () => {
    vi.mocked(chatApi.ticket).mockRejectedValueOnce(new Error('service down'))

    const socket = start()
    await vi.advanceTimersByTimeAsync(1_000)
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))

    // Two ticket calls: the failed one and the one that worked.
    expect(ticketCount).toBe(1)
    expect(vi.mocked(chatApi.ticket).mock.calls).toHaveLength(2)
    socket.stop()
  })

  it('writes a frame only while the socket is open, and says which happened', async () => {
    const socket = start()
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))
    const ws = FakeWebSocket.instances[0]

    const frame = { type: 'message', client_msg_id: 'cmid-1', body: 'hi' } as const

    // Connected but not open yet: nothing is written, and the caller is told.
    expect(socket.send(frame)).toBe(false)
    expect(ws.sent).toHaveLength(0)

    ws.open()
    expect(socket.send(frame)).toBe(true)
    expect(JSON.parse(ws.sent[0])).toEqual(frame)

    ws.drop()
    expect(socket.send(frame)).toBe(false)
    expect(ws.sent).toHaveLength(1)
    socket.stop()
  })

  it('dispatches known frames and drops unknown ones', async () => {
    const socket = start()
    await vi.waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1))
    const ws = FakeWebSocket.instances[0]
    ws.open()

    ws.deliver(JSON.stringify({ type: 'message', id: 1, body: 'hi' }))
    ws.deliver(JSON.stringify({ type: 'error', code: 'rate_limited', message: 'slow down' }))
    // A type this build does not know is a message meant for a newer one: it is
    // dropped, not thrown on.
    ws.deliver(JSON.stringify({ type: 'read_receipt', user_id: 'u1' }))
    ws.deliver('not json at all')
    ws.deliver(JSON.stringify(null))

    expect(frames).toHaveLength(2)
    expect(frames[0]).toMatchObject({ type: 'message' })
    expect(frames[1]).toMatchObject({ type: 'error', code: 'rate_limited' })
    socket.stop()
  })
})
