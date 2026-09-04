import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'

import { keys } from '@/hooks/queryKeys'
import * as chatApi from '@/lib/api/chat'
import { ChatSocket, newClientMsgId, type ConnectionState } from '@/lib/ws/chatSocket'
import type { ChatFrame, ChatMessage } from '@/lib/types'

/** A message that has been typed but not yet echoed back by the server. */
export interface PendingMessage {
  client_msg_id: string
  body: string
  created_at: string
  /** Set when no echo arrived in time, so the row can offer a retry. */
  failed?: boolean
}

/** A message waiting for a connection to carry it. */
interface QueuedMessage {
  client_msg_id: string
  body: string
}

/** An error frame the server sent about something the user just did. */
export interface ChatError {
  code: string
  message: string
}

/**
 * How long an optimistic bubble waits for its echo before it says "Not sent".
 *
 * It is a display deadline and nothing more: the message is not cancelled, no
 * retry is issued, and if the echo turns up at eleven seconds the row is
 * reconciled exactly as it would have been at one. Ten seconds is long enough
 * that a slow round trip does not accuse the service of losing a message, and
 * short enough that a user who has actually lost their connection finds out
 * before they have typed three more.
 */
const NOT_SENT_AFTER_MS = 10_000

/**
 * One room: its history, its live socket, and the optimistic send.
 *
 * Every message leaves over the socket. The chat service has no REST write path
 * — `POST /api/chat/rooms/{tripId}/messages` is a 405 — so there is no fallback
 * to take when the connection is down, and none is wanted: a message typed
 * while the socket is reconnecting is held here, in order, and flushed when it
 * reopens.
 *
 * The reconciliation rule, in one place so it cannot be implemented twice
 * differently: a pending row is identified by its `client_msg_id` and by
 * nothing else. It is dropped the moment a `message` frame carrying that id
 * arrives. Never by body text — two people can send "yes", one person can send
 * "yes" twice, and matching on text would make either of those eat the wrong
 * bubble.
 */
export function useChatRoom(tripId: string | undefined) {
  const queryClient = useQueryClient()
  const [connection, setConnection] = useState<ConnectionState>('closed')
  const [pending, setPending] = useState<PendingMessage[]>([])
  /** Messages that arrived on the socket since the last history fetch. */
  const [live, setLive] = useState<ChatMessage[]>([])
  const [error, setError] = useState<ChatError | null>(null)

  const socketRef = useRef<ChatSocket | null>(null)
  /** Messages that have no open socket to leave on, oldest first. */
  const queueRef = useRef<QueuedMessage[]>([])
  /** The "Not sent" deadline for each pending row, by client_msg_id. */
  const timersRef = useRef(new Map<string, ReturnType<typeof setTimeout>>())

  const history = useInfiniteQuery({
    queryKey: keys.chat.messages(tripId ?? ''),
    queryFn: ({ pageParam }) => chatApi.messages(tripId as string, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
    enabled: Boolean(tripId),
    retry: false,
  })

  const clearTimer = useCallback((clientMsgId: string) => {
    const timer = timersRef.current.get(clientMsgId)
    if (timer === undefined) return
    clearTimeout(timer)
    timersRef.current.delete(clientMsgId)
  }, [])

  /** (Re)starts one row's echo deadline. */
  const armTimer = useCallback(
    (clientMsgId: string) => {
      clearTimer(clientMsgId)
      timersRef.current.set(
        clientMsgId,
        setTimeout(() => {
          timersRef.current.delete(clientMsgId)
          setPending((rows) =>
            rows.map((row) =>
              row.client_msg_id === clientMsgId ? { ...row, failed: true } : row,
            ),
          )
        }, NOT_SENT_AFTER_MS),
      )
    },
    [clearTimer],
  )

  /** Forgets a pending row entirely: the bubble, its deadline and its queue slot. */
  const forget = useCallback(
    (clientMsgId: string) => {
      clearTimer(clientMsgId)
      queueRef.current = queueRef.current.filter((row) => row.client_msg_id !== clientMsgId)
      setPending((rows) => rows.filter((row) => row.client_msg_id !== clientMsgId))
    },
    [clearTimer],
  )

  const ingest = useCallback(
    (message: ChatMessage) => {
      // The echo. Matched on the id the client generated and on nothing else.
      if (message.client_msg_id) forget(message.client_msg_id)
      setLive((rows) => {
        // A socket reconnect can replay the tail of a room and the sender sees
        // their own message come back like everybody else's, so a duplicate id
        // is expected rather than exceptional.
        if (rows.some((row) => row.id === message.id)) return rows
        return [...rows, message]
      })
    },
    [forget],
  )

  /**
   * Puts one message on the wire, or holds it until there is a wire.
   *
   * The deadline is armed here rather than at the composer, so a message that
   * spent a minute in the queue gets a fresh ten seconds to be echoed once it
   * has actually left — the deadline is about a lost echo, not about how long
   * the user has been offline.
   */
  const dispatch = useCallback(
    (entry: QueuedMessage): boolean => {
      armTimer(entry.client_msg_id)
      setPending((rows) =>
        rows.map((row) =>
          row.client_msg_id === entry.client_msg_id ? { ...row, failed: false } : row,
        ),
      )

      const sent =
        socketRef.current?.send({
          type: 'message',
          client_msg_id: entry.client_msg_id,
          body: entry.body,
        }) ?? false

      if (!sent) {
        // Queued at most once, however many times it is attempted: a retry
        // while offline must not make the message go twice when it reopens.
        queueRef.current = [
          ...queueRef.current.filter((row) => row.client_msg_id !== entry.client_msg_id),
          entry,
        ]
      }
      return sent
    },
    [armTimer],
  )

  /**
   * Sends everything the queue is holding, oldest first.
   *
   * In order and stopping at the first refusal, because a room where the second
   * message a user typed arrives before the first is worse than one where both
   * arrive late.
   */
  const flush = useCallback(() => {
    const queued = queueRef.current
    queueRef.current = []
    for (let index = 0; index < queued.length; index += 1) {
      if (!dispatch(queued[index])) {
        // dispatch has re-queued the one that failed; the rest follow it.
        queueRef.current = queueRef.current.concat(queued.slice(index + 1))
        return
      }
    }
  }, [dispatch])

  const onFrame = useCallback(
    (frame: ChatFrame) => {
      switch (frame.type) {
        case 'message':
          ingest(frame)
          break
        case 'error':
          // Shown inline and never acted on further. A `rate_limited` refusal in
          // particular is the user typing faster than the room allows, which
          // corrects itself in seconds — closing the socket over it would cost
          // them the conversation to save them nothing.
          setError({ code: frame.code, message: frame.message })
          break
        default:
          // presence and typing are not rendered yet; a frame this build does
          // not use is not a frame it should fall over on.
          break
      }
    },
    [ingest],
  )

  const onState = useCallback(
    (state: ConnectionState) => {
      setConnection(state)
      if (state === 'open') flush()
    },
    [flush],
  )

  // One socket per room, torn down on navigation. Every handler above is
  // stable, so this effect runs when the room changes and at no other time —
  // which matters, because each run costs a single-use ticket.
  useEffect(() => {
    if (!tripId) return

    setLive([])
    setPending([])
    setError(null)
    queueRef.current = []

    const socket = new ChatSocket(tripId, { onState, onFrame })
    socketRef.current = socket
    socket.start()

    const timers = timersRef.current
    return () => {
      socket.stop()
      socketRef.current = null
      timers.forEach((timer) => clearTimeout(timer))
      timers.clear()
    }
  }, [tripId, onState, onFrame])

  /**
   * Refetches history after a reconnect.
   *
   * A drop is exactly the window in which messages are missed: they were said
   * while the socket was down, so no frame will ever deliver them. The
   * reconnect is the signal to go and ask.
   */
  const wasOpen = useRef(false)
  useEffect(() => {
    if (connection === 'open' && wasOpen.current) {
      setLive([])
      void queryClient.invalidateQueries({ queryKey: keys.chat.messages(tripId ?? '') })
    }
    if (connection === 'open') wasOpen.current = true
  }, [connection, queryClient, tripId])

  const send = useCallback(
    (body: string) => {
      const text = body.trim()
      if (!text || !tripId) return

      const clientMsgId = newClientMsgId()
      setError(null)
      setPending((rows) => [
        ...rows,
        { client_msg_id: clientMsgId, body: text, created_at: new Date().toISOString() },
      ])
      dispatch({ client_msg_id: clientMsgId, body: text })
    },
    [tripId, dispatch],
  )

  const retry = useCallback(
    (clientMsgId: string) => {
      const row = pending.find((candidate) => candidate.client_msg_id === clientMsgId)
      if (!row) return
      // The same client_msg_id, deliberately: if the first attempt landed and
      // only its echo was lost, the sender is retrying a message the room
      // already has, and the id is what lets the echo of either attempt settle
      // this row instead of leaving a duplicate on screen.
      dispatch({ client_msg_id: clientMsgId, body: row.body })
    },
    [pending, dispatch],
  )

  const discard = useCallback((clientMsgId: string) => forget(clientMsgId), [forget])

  const dismissError = useCallback(() => setError(null), [])

  /**
   * Oldest first, which is the order a thread reads in.
   *
   * History arrives newest-first and paged backwards through time, so the pages
   * are flattened and reversed, then the live tail is appended.
   */
  const messages = useMemo(() => {
    const fromHistory = (history.data?.pages ?? []).flatMap((page) => page.items)
    const seen = new Set<number>()
    const ordered: ChatMessage[] = []

    for (const message of [...fromHistory].reverse().concat(live)) {
      if (seen.has(message.id)) continue
      seen.add(message.id)
      ordered.push(message)
    }
    return ordered
  }, [history.data, live])

  return { history, messages, pending, connection, error, send, retry, discard, dismissError }
}
