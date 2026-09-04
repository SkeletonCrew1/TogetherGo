import { StrictMode, type ReactNode } from 'react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Chat } from './Chat'
import * as chatApi from '@/lib/api/chat'

/**
 * The send path, as the composer actually performs it.
 *
 * These are component tests rather than hook tests because the bug they exist
 * to prevent was a component-level one: the submit handler called an HTTP
 * endpoint that does not exist (`POST /api/chat/rooms/{tripId}/messages` is
 * GET-only history and answers 405), so nothing was ever delivered and every
 * bubble ended up saying "Not sent". The assertions are therefore about what
 * leaves the browser — a frame on the socket, and no request at all.
 */

const TRIP_ID = '3f1f0a4e-5c4a-4a2f-9f43-1a2b3c4d5e6f'
const ME = '11111111-1111-4111-8111-111111111111'

vi.mock('@/lib/api/chat', () => ({
  rooms: vi.fn(),
  messages: vi.fn(),
  ticket: vi.fn(),
}))

// The thread resolves sender ids to names through identity's public profiles.
// Stubbed here so that a test about the socket is not also a test about that.
vi.mock('@/lib/api/users', () => ({
  publicProfile: vi.fn(async () => {
    throw new Error('no profile in this test')
  }),
}))

vi.mock('@/auth/AuthContext', () => ({
  useCurrentUser: () => ({ id: ME, full_name: 'Me', email: 'me@example.test' }),
}))

/**
 * A WebSocket that records what was written to it.
 *
 * `readyState` is real, because "is the socket open" is the branch the offline
 * queue turns on and a double that is always open cannot exercise it.
 */
class FakeWebSocket {
  static instances: FakeWebSocket[] = []
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null

  readonly url: string
  readyState = FakeWebSocket.CONNECTING
  sent: string[] = []

  constructor(url: string) {
    this.url = url
    FakeWebSocket.instances.push(this)
  }

  send(data: string) {
    this.sent.push(data)
  }

  close() {
    this.readyState = FakeWebSocket.CLOSED
  }

  /** The frames the client wrote, decoded. */
  get frames(): { type: string; client_msg_id: string; body: string }[] {
    return this.sent.map((raw) => JSON.parse(raw))
  }

  open() {
    this.readyState = FakeWebSocket.OPEN
    this.onopen?.()
  }

  drop() {
    this.readyState = FakeWebSocket.CLOSED
    this.onclose?.()
  }

  deliver(frame: unknown) {
    this.onmessage?.({ data: JSON.stringify(frame) })
  }
}

let fetchSpy: ReturnType<typeof vi.fn>
let nextUuid = 0

beforeEach(() => {
  FakeWebSocket.instances.length = 0
  nextUuid = 0

  vi.useFakeTimers()
  vi.stubGlobal('WebSocket', FakeWebSocket)

  // jsdom has no layout, so this is absent rather than a no-op. The thread
  // autoscrolls on every render.
  Element.prototype.scrollIntoView = vi.fn()

  // Nothing in the send path may reach the network. The spy is the assertion.
  fetchSpy = vi.fn(async () => {
    throw new Error('no request should be made')
  })
  vi.stubGlobal('fetch', fetchSpy)

  // Readable ids, so a failure message names the message it is about.
  vi.stubGlobal('crypto', {
    ...globalThis.crypto,
    randomUUID: () => {
      nextUuid += 1
      return `cmid-${nextUuid}`
    },
  })

  vi.mocked(chatApi.rooms).mockResolvedValue({ items: [], next_cursor: null })
  vi.mocked(chatApi.messages).mockResolvedValue({ items: [], next_cursor: null })
  let ticketCount = 0
  vi.mocked(chatApi.ticket).mockImplementation(async () => {
    ticketCount += 1
    return { ticket: `tkt-${ticketCount}`, expires_in: 30 }
  })
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

function wrap(children: ReactNode, strict = false) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  const tree = (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/chat/${TRIP_ID}`]}>
        <Routes>
          <Route path="/chat/:tripId" element={children} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )
  return strict ? <StrictMode>{tree}</StrictMode> : tree
}

/**
 * Renders the room and returns the socket it opened.
 *
 * One tick, not a `waitFor`: testing-library's does not know about vitest's
 * fake timers and would spin on real ones while the deferred connect and the
 * ticket request sat in a queue nothing was advancing.
 */
async function openRoom({ strict = false } = {}) {
  render(wrap(<Chat />, strict))
  await tick(1)
  expect(FakeWebSocket.instances.length).toBeGreaterThan(0)
  return FakeWebSocket.instances[FakeWebSocket.instances.length - 1]
}

/** Types into the composer and hits Enter, which is what Send does. */
function compose(text: string) {
  const composer = screen.getByLabelText('Message')
  fireEvent.change(composer, { target: { value: text } })
  fireEvent.keyDown(composer, { key: 'Enter' })
}

/** A `message` frame as services/chat writes it: flat, with the id echoed. */
function echoOf(clientMsgId: string, body: string, id: number) {
  return {
    type: 'message',
    id,
    trip_id: TRIP_ID,
    sender_id: ME,
    body,
    created_at: '2026-08-30T12:00:00Z',
    client_msg_id: clientMsgId,
  }
}

async function tick(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

describe('the chat composer', () => {
  it('sends over the socket and makes no request at all', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('we leave at six')

    expect(socket.frames).toEqual([
      { type: 'message', client_msg_id: 'cmid-1', body: 'we leave at six' },
    ])
    // The bug this replaces: a POST to a GET-only endpoint, answered 405.
    expect(fetchSpy).not.toHaveBeenCalled()

    // And the bubble is on screen immediately, pending its echo.
    expect(screen.getByText('we leave at six')).toBeDefined()
    expect(screen.getByText('Sending…')).toBeDefined()
  })

  it('trims the body and reuses nothing between two sends', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('   first   ')
    compose('second')

    expect(socket.frames.map((frame) => frame.body)).toEqual(['first', 'second'])
    expect(socket.frames[0].client_msg_id).not.toBe(socket.frames[1].client_msg_id)
  })

  it('replaces the pending bubble when the echo carrying its id arrives', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('we leave at six')
    const { client_msg_id: clientMsgId } = socket.frames[0]

    act(() => socket.deliver(echoOf(clientMsgId, 'we leave at six', 42)))

    // One bubble, not two: the optimistic row was replaced by the server's
    // copy rather than left beside it.
    expect(screen.getAllByText('we leave at six')).toHaveLength(1)
    // And it is no longer pending, so neither pending state is on screen.
    expect(screen.queryByText('Sending…')).toBeNull()
    expect(screen.queryByText(/Not sent/)).toBeNull()

    // The deadline was cancelled with it: ten seconds later nothing has
    // decided the message failed.
    await tick(15_000)
    expect(screen.queryByText(/Not sent/)).toBeNull()
    expect(screen.getAllByText('we leave at six')).toHaveLength(1)
  })

  it('ignores an echo for a different message rather than matching on text', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('yes')
    const mine = socket.frames[0].client_msg_id

    // Somebody else says the same word. Matching on body would settle the
    // sender's pending row against a message that is not theirs.
    act(() =>
      socket.deliver({
        type: 'message',
        id: 7,
        trip_id: TRIP_ID,
        sender_id: '22222222-2222-4222-8222-222222222222',
        body: 'yes',
        created_at: '2026-08-30T12:00:00Z',
      }),
    )

    expect(screen.getByText('Sending…')).toBeDefined()
    expect(screen.getAllByText('yes')).toHaveLength(2)

    act(() => socket.deliver(echoOf(mine, 'yes', 8)))
    expect(screen.queryByText('Sending…')).toBeNull()
    expect(screen.getAllByText('yes')).toHaveLength(2)
  })

  it('says "Not sent" only after ten seconds without an echo', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('anyone there')

    await tick(9_000)
    expect(screen.getByText('Sending…')).toBeDefined()
    expect(screen.queryByText(/Not sent/)).toBeNull()

    await tick(2_000)
    expect(screen.getByText(/Not sent/)).toBeDefined()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeDefined()
    expect(screen.getByRole('button', { name: 'Discard' })).toBeDefined()
  })

  it('retries over the socket with the same client_msg_id', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('anyone there')
    const first = socket.frames[0].client_msg_id

    await tick(11_000)
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))

    expect(socket.frames).toHaveLength(2)
    // The same id, deliberately: if the first attempt was persisted and only
    // its echo was lost, the service and every other client can recognise the
    // retry instead of the room ending up with the message twice.
    expect(socket.frames[1]).toEqual({
      type: 'message',
      client_msg_id: first,
      body: 'anyone there',
    })
    expect(fetchSpy).not.toHaveBeenCalled()

    // And one echo settles it, whichever attempt produced it.
    act(() => socket.deliver(echoOf(first, 'anyone there', 12)))
    expect(screen.getAllByText('anyone there')).toHaveLength(1)
    expect(screen.queryByText(/Not sent/)).toBeNull()
  })

  it('discards a failed bubble without sending anything', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('never mind')
    await tick(11_000)
    fireEvent.click(screen.getByRole('button', { name: 'Discard' }))

    expect(screen.queryByText('never mind')).toBeNull()
    expect(socket.frames).toHaveLength(1)
  })

  it('queues what is typed while the socket is closed and flushes it in order', async () => {
    const first = await openRoom()
    act(() => first.open())
    act(() => first.drop())

    compose('one')
    compose('two')

    // Nothing left the browser: the socket is closed and there is no HTTP
    // fallback to take.
    expect(first.frames).toHaveLength(0)
    expect(fetchSpy).not.toHaveBeenCalled()
    expect(screen.getByText('one')).toBeDefined()
    expect(screen.getByText('two')).toBeDefined()

    // The reconnect is behind a jittered backoff of at most a second, and it
    // fetches a fresh ticket on the way.
    await tick(2_000)
    expect(FakeWebSocket.instances).toHaveLength(2)
    const second = FakeWebSocket.instances[1]

    act(() => second.open())

    expect(second.frames.map((frame) => frame.body)).toEqual(['one', 'two'])
    expect(vi.mocked(chatApi.ticket)).toHaveBeenCalledTimes(2)

    // Once each, not once per reconnect: a queue that is not drained as it is
    // flushed sends everything again on the next drop.
    act(() => second.drop())
    await tick(2_000)
    expect(FakeWebSocket.instances).toHaveLength(3)
    const third = FakeWebSocket.instances[2]
    act(() => third.open())

    expect(third.frames).toHaveLength(0)
  })

  it('shows an error frame inline and keeps the composer usable after a rate limit', async () => {
    const socket = await openRoom()
    act(() => socket.open())

    compose('one')
    act(() =>
      socket.deliver({
        type: 'error',
        code: 'rate_limited',
        message: 'You are sending messages too quickly. Wait a moment and try again.',
      }),
    )

    expect(screen.getByText(/sending messages too quickly/i)).toBeDefined()

    // Not disconnected, and not disabled: the socket is still open and the next
    // message goes.
    const composer = screen.getByLabelText('Message') as HTMLTextAreaElement
    expect(composer.disabled).toBe(false)
    compose('two')
    expect(socket.frames.map((frame) => frame.body)).toEqual(['one', 'two'])
    // Sending again clears the last refusal.
    expect(screen.queryByText(/sending messages too quickly/i)).toBeNull()
  })
})

describe('the room connection', () => {
  /**
   * The StrictMode double mount, which was spending a single-use ticket on
   * every page load: two POST /api/chat/tickets in the same millisecond, and
   * one socket to show for them.
   */
  it('spends one ticket and opens one socket under StrictMode', async () => {
    await openRoom({ strict: true })
    await tick(100)

    expect(vi.mocked(chatApi.ticket)).toHaveBeenCalledTimes(1)
    expect(FakeWebSocket.instances).toHaveLength(1)
  })
})
