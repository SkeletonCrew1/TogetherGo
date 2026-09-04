import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'

import { useChatRoom, type PendingMessage } from './useChatRoom'
import { useCurrentUser } from '@/auth/AuthContext'
import { Alert } from '@/components/ui/Alert'
import { Avatar } from '@/components/ui/Avatar'
import { Button } from '@/components/ui/Button'
import { EmptyState, ServiceUnavailableState } from '@/components/ui/EmptyState'
import { Skeleton } from '@/components/ui/Skeleton'
import { keys } from '@/hooks/queryKeys'
import { useParticipantProfiles } from '@/hooks/useParticipantProfiles'
import * as chatApi from '@/lib/api/chat'
import { ApiError, errorMessage } from '@/lib/api/errors'
import { cx, formatRelative, formatTime } from '@/lib/format'
import { CHAT_ERROR_RATE_LIMITED, type ChatMessage, type ChatRoom } from '@/lib/types'
import type { ConnectionState } from '@/lib/ws/chatSocket'

const CHAT_REFERENCE = 'services/chat/'

export function Chat() {
  const { tripId } = useParams()

  const roomsQuery = useQuery({
    queryKey: keys.chat.rooms,
    queryFn: () => chatApi.rooms(),
    retry: false,
  })

  const room = useChatRoom(tripId)

  // A 404 here means the chat service is not deployed, not that the user has no
  // rooms. The distinction decides which empty state is honest.
  const notDeployed =
    (roomsQuery.error instanceof ApiError && roomsQuery.error.status === 404) ||
    (room.history.error instanceof ApiError && room.history.error.status === 404)

  return (
    <div className="mx-auto max-w-6xl px-4 py-6">
      <h1 className="text-2xl font-semibold text-slate-900">Chat</h1>

      {notDeployed && (
        <div className="mt-5">
          <ServiceUnavailableState
            service="chat"
            what={
              'The chat service is not answering on /api/chat/* and /ws/chat/*. ' +
              'The room list, the thread, the composer, the ticket handshake and the reconnect ' +
              'backoff are all built and wired — they need the service to be up.'
            }
            reference={CHAT_REFERENCE}
          />
        </div>
      )}

      {!notDeployed && (
        <div className="mt-5 grid h-[calc(100vh-12rem)] gap-4 lg:grid-cols-[18rem,1fr]">
          <RoomList
            rooms={roomsQuery.data?.items ?? []}
            isLoading={roomsQuery.isLoading}
            error={roomsQuery.error}
            activeTripId={tripId}
          />

          {tripId ? (
            <Thread tripId={tripId} room={room} />
          ) : (
            <div className="flex items-center justify-center rounded-xl border border-slate-200 bg-white">
              <EmptyState
                title="Pick a room"
                description="Every trip you are approved for has one. Approval is what grants access."
              />
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function RoomList({
  rooms,
  isLoading,
  error,
  activeTripId,
}: {
  rooms: ChatRoom[]
  isLoading: boolean
  error: unknown
  activeTripId: string | undefined
}) {
  return (
    <aside className="flex min-h-0 flex-col overflow-hidden rounded-xl border border-slate-200 bg-white">
      <h2 className="border-b border-slate-100 px-4 py-3 text-sm font-semibold text-slate-700">
        Rooms
      </h2>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {isLoading ? (
          <div className="space-y-2 p-3">
            <Skeleton className="h-12 w-full rounded-lg" />
            <Skeleton className="h-12 w-full rounded-lg" />
            <Skeleton className="h-12 w-full rounded-lg" />
          </div>
        ) : error ? (
          <div className="p-3">
            <Alert>{errorMessage(error)}</Alert>
          </div>
        ) : rooms.length === 0 ? (
          <p className="p-4 text-sm text-slate-500">
            No rooms yet. You get one for every trip you organize or are approved for.
          </p>
        ) : (
          <ul>
            {rooms.map((entry) => (
              <li key={entry.trip_id}>
                <Link
                  to={`/chat/${entry.trip_id}`}
                  className={cx(
                    'block border-b border-slate-100 px-4 py-3 transition-colors',
                    entry.trip_id === activeTripId ? 'bg-brand-50' : 'hover:bg-slate-50',
                  )}
                >
                  <div className="flex items-baseline justify-between gap-2">
                    <span className="truncate text-sm font-medium text-slate-900">
                      {entry.title}
                    </span>
                    {entry.unread_count > 0 && (
                      <span className="shrink-0 rounded-full bg-brand-600 px-1.5 py-0.5 text-[11px] font-semibold text-white">
                        {entry.unread_count}
                      </span>
                    )}
                  </div>
                  <p className="mt-0.5 truncate text-xs text-slate-500">
                    {entry.last_message ? entry.last_message.body : 'No messages yet'}
                  </p>
                  {!entry.is_open && (
                    <p className="mt-0.5 text-[11px] text-slate-400">Closed to new messages</p>
                  )}
                </Link>
              </li>
            ))}
          </ul>
        )}
      </div>
    </aside>
  )
}

function ConnectionBadge({ state }: { state: ConnectionState }) {
  if (state === 'open') return null

  const copy: Record<Exclude<ConnectionState, 'open'>, string> = {
    connecting: 'Connecting…',
    reconnecting: 'Reconnecting…',
    closed: 'Not connected',
  }

  return (
    <span
      role="status"
      className={cx(
        'rounded-full px-2 py-0.5 text-xs font-medium',
        state === 'closed' ? 'bg-slate-100 text-slate-600' : 'bg-amber-100 text-amber-900',
      )}
    >
      {copy[state]}
    </span>
  )
}

function Thread({ tripId, room }: { tripId: string; room: ReturnType<typeof useChatRoom> }) {
  const me = useCurrentUser()
  const [draft, setDraft] = useState('')
  const scrollRef = useRef<HTMLDivElement>(null)
  const bottomRef = useRef<HTMLDivElement>(null)

  const { messages, pending, connection, error, history, send, retry, discard, dismissError } = room

  // The service sends a sender_id and nothing else about the person — trip owns
  // seats, identity owns people, and there is no join across that boundary
  // (CLAUDE.md). Names come from identity's public profiles, cached by the
  // query client so a room of six is six requests once, not one per message.
  const senderIds = useMemo(() => messages.map((message) => message.sender_id), [messages])
  const { profiles } = useParticipantProfiles(senderIds)

  /**
   * Follows the bottom, but only when the reader is already there.
   *
   * Scrolling someone to the bottom while they are reading history is worse
   * than not autoscrolling at all, and paging older messages in would do
   * exactly that on every fetch.
   */
  useEffect(() => {
    const container = scrollRef.current
    if (!container) return
    const distanceFromBottom =
      container.scrollHeight - container.scrollTop - container.clientHeight
    if (distanceFromBottom < 200) {
      bottomRef.current?.scrollIntoView({ block: 'end' })
    }
  }, [messages.length, pending.length])

  /**
   * The whole send path: one call onto the open socket, and no HTTP.
   *
   * There is no request to make. `POST /api/chat/rooms/{tripId}/messages` does
   * not exist — the endpoint is GET-only history — so a composer that posted
   * got a 405 and every bubble ended up saying "Not sent".
   */
  const submit = (event: React.FormEvent) => {
    event.preventDefault()
    const text = draft.trim()
    if (!text) return
    setDraft('')
    send(text)
  }

  return (
    <section className="flex min-h-0 flex-col overflow-hidden rounded-xl border border-slate-200 bg-white">
      <header className="flex items-center gap-3 border-b border-slate-100 px-4 py-3">
        <Link to={`/trip/${tripId}`} className="text-sm font-medium text-brand-700 hover:underline">
          Open trip
        </Link>
        <div className="ml-auto">
          <ConnectionBadge state={connection} />
        </div>
      </header>

      <div ref={scrollRef} className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
        {history.hasNextPage && (
          <div className="mb-4 flex justify-center">
            <Button
              size="sm"
              variant="secondary"
              isLoading={history.isFetchingNextPage}
              onClick={() => void history.fetchNextPage()}
            >
              Load earlier messages
            </Button>
          </div>
        )}

        {history.isLoading ? (
          <div className="space-y-3">
            <Skeleton className="h-12 w-2/3 rounded-2xl" />
            <Skeleton className="ml-auto h-12 w-1/2 rounded-2xl" />
            <Skeleton className="h-12 w-3/5 rounded-2xl" />
          </div>
        ) : history.error ? (
          <Alert>{errorMessage(history.error)}</Alert>
        ) : messages.length === 0 && pending.length === 0 ? (
          <EmptyState
            title="Nothing said yet"
            description="This room belongs to everyone approved for the trip."
          />
        ) : (
          <ol className="space-y-3">
            {messages.map((message) => (
              <MessageRow
                key={message.id}
                message={message}
                isMine={message.sender_id === me.id}
                authorName={profiles.get(message.sender_id)?.full_name ?? null}
                authorPhotoUrl={profiles.get(message.sender_id)?.photo_url ?? null}
              />
            ))}
            {pending.map((message) => (
              <PendingRow
                key={message.client_msg_id}
                message={message}
                onRetry={() => retry(message.client_msg_id)}
                onDiscard={() => discard(message.client_msg_id)}
              />
            ))}
          </ol>
        )}

        <div ref={bottomRef} />
      </div>

      <form onSubmit={submit} className="border-t border-slate-100 p-3">
        {error && (
          <div className="mb-2">
            {/* A rate limit is a warning and not a failure: the socket is still
                open, the composer is still usable, and the next message a few
                seconds from now will go. Anything else the service refuses is
                an error the sender has to see. */}
            <Alert tone={error.code === CHAT_ERROR_RATE_LIMITED ? 'warning' : 'error'}>
              <span className="flex items-start gap-2">
                <span className="flex-1">{error.message}</span>
                <button
                  type="button"
                  onClick={dismissError}
                  className="shrink-0 underline underline-offset-2"
                >
                  Dismiss
                </button>
              </span>
            </Alert>
          </div>
        )}

        <div className="flex items-end gap-2">
          <label htmlFor="composer" className="sr-only">
            Message
          </label>
          <textarea
            id="composer"
            rows={1}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={(event) => {
              // Enter sends, Shift+Enter is a newline — the convention every
              // chat client uses, and the reason this is not a plain input.
              if (event.key === 'Enter' && !event.shiftKey) {
                event.preventDefault()
                submit(event)
              }
            }}
            placeholder="Write a message…"
            maxLength={4000}
            className="max-h-32 flex-1 resize-y rounded-xl border-0 px-3 py-2 text-slate-900 shadow-sm ring-1 ring-inset ring-slate-300 placeholder:text-slate-400 focus:ring-2 focus:ring-inset focus:ring-brand-600"
          />
          <Button type="submit" disabled={!draft.trim()}>
            Send
          </Button>
        </div>
        {connection !== 'open' && (
          <p className="mt-1.5 text-xs text-slate-500">
            {/* Deliberately not disabled. The message is held in order and sent
                the moment the socket is back, which is a better answer than a
                greyed-out composer and a sentence the user has to retype. */}
            You can still write. Anything you send is queued and delivered in order as soon as the
            connection is back.
          </p>
        )}
      </form>
    </section>
  )
}

function MessageRow({
  message,
  isMine,
  authorName,
  authorPhotoUrl,
}: {
  message: ChatMessage
  isMine: boolean
  authorName: string | null
  authorPhotoUrl: string | null
}) {
  return (
    <li className={cx('flex gap-2', isMine && 'flex-row-reverse')}>
      <Avatar name={authorName} photoUrl={authorPhotoUrl} size="xs" />
      <div className={cx('max-w-[75%]', isMine && 'text-right')}>
        {!isMine && (
          <p className="mb-0.5 text-xs font-medium text-slate-500">{authorName ?? 'Traveller'}</p>
        )}
        <div
          className={cx(
            'inline-block whitespace-pre-wrap rounded-2xl px-3 py-2 text-sm',
            isMine ? 'bg-brand-600 text-white' : 'bg-slate-100 text-slate-900',
          )}
        >
          {message.body}
        </div>
        <p className="mt-0.5 text-[11px] text-slate-400" title={formatRelative(message.created_at)}>
          {formatTime(message.created_at)}
        </p>
      </div>
    </li>
  )
}

/** An optimistic row: dimmed while in flight, actionable once it has failed. */
function PendingRow({
  message,
  onRetry,
  onDiscard,
}: {
  message: PendingMessage
  onRetry: () => void
  onDiscard: () => void
}) {
  return (
    <li className="flex flex-row-reverse gap-2" data-pending={message.client_msg_id}>
      <div className="max-w-[75%] text-right">
        <div
          className={cx(
            'inline-block whitespace-pre-wrap rounded-2xl px-3 py-2 text-sm',
            message.failed ? 'bg-rose-100 text-rose-900' : 'bg-brand-600/60 text-white',
          )}
        >
          {message.body}
        </div>
        {message.failed ? (
          <p className="mt-0.5 text-[11px] text-rose-600">
            Not sent.{' '}
            <button type="button" onClick={onRetry} className="underline underline-offset-2">
              Retry
            </button>{' '}
            ·{' '}
            <button type="button" onClick={onDiscard} className="underline underline-offset-2">
              Discard
            </button>
          </p>
        ) : (
          <p className="mt-0.5 text-[11px] text-slate-400">Sending…</p>
        )}
      </div>
    </li>
  )
}
