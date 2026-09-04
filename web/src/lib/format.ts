import type { Category, JoinRequestStatus, TripStatus } from '@/lib/types'

/**
 * Dates are rendered in the viewer's own zone. Everything on the wire is UTC
 * (CLAUDE.md), and a trip that starts at 09:00 local is a trip whose organizer
 * meant 09:00 local — showing them a Z timestamp would be technically honest
 * and practically useless.
 */
const dayMonth = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' })
const dayMonthYear = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
})
const timeOnly = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' })
const full = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })

export const formatDate = (iso: string) => dayMonthYear.format(new Date(iso))
export const formatTime = (iso: string) => timeOnly.format(new Date(iso))
export const formatDateTime = (iso: string) => full.format(new Date(iso))

/** "12 – 15 Mar 2026", collapsing the parts the two ends share. */
export function formatDateRange(startIso: string, endIso: string): string {
  const start = new Date(startIso)
  const end = new Date(endIso)
  if (start.toDateString() === end.toDateString()) return dayMonthYear.format(start)
  if (start.getFullYear() === end.getFullYear()) {
    return `${dayMonth.format(start)} – ${dayMonthYear.format(end)}`
  }
  return `${dayMonthYear.format(start)} – ${dayMonthYear.format(end)}`
}

/** Whole days, rounded up: a trip ending the morning after it starts is 2 days. */
export function durationDays(startIso: string, endIso: string): number {
  const ms = new Date(endIso).getTime() - new Date(startIso).getTime()
  return Math.max(1, Math.ceil(ms / 86_400_000))
}

/** Relative time for a chat thread. Falls back to a date past a week. */
export function formatRelative(iso: string): string {
  const seconds = Math.round((Date.now() - new Date(iso).getTime()) / 1000)
  if (seconds < 60) return 'just now'
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`
  if (seconds < 604_800) return `${Math.floor(seconds / 86_400)}d ago`
  return formatDate(iso)
}

/** For an ISO date the browser's `<input type="date">` will accept. */
export function toDateInput(iso: string | null): string {
  if (!iso) return ''
  return new Date(iso).toISOString().slice(0, 10)
}

/**
 * A local `datetime-local` value from a UTC instant, and back.
 *
 * `new Date(iso).toISOString().slice(0,16)` is the tempting one-liner and it is
 * wrong by the viewer's UTC offset — it would show a Berlin organizer 07:00 for
 * a trip they set to start at 09:00.
 */
export function toDateTimeInput(iso: string | null): string {
  if (!iso) return ''
  const date = new Date(iso)
  const offsetMs = date.getTimezoneOffset() * 60_000
  return new Date(date.getTime() - offsetMs).toISOString().slice(0, 16)
}

export function fromDateTimeInput(local: string): string {
  // `new Date("2026-09-01T09:00")` is parsed as local time, which is what the
  // control means, and toISOString converts it to the UTC the API wants.
  return new Date(local).toISOString()
}

export const CATEGORY_LABELS: Record<Category, string> = {
  nature: 'Nature',
  city: 'City',
  abroad: 'Abroad',
  hiking: 'Hiking',
  food: 'Food',
  other: 'Other',
}

export const CATEGORY_STYLES: Record<Category, string> = {
  nature: 'bg-emerald-100 text-emerald-800',
  city: 'bg-sky-100 text-sky-800',
  abroad: 'bg-violet-100 text-violet-800',
  hiking: 'bg-amber-100 text-amber-800',
  food: 'bg-rose-100 text-rose-800',
  other: 'bg-slate-100 text-slate-700',
}

export const TRIP_STATUS_LABELS: Record<TripStatus, string> = {
  draft: 'Draft',
  recruiting: 'Planned',
  in_progress: 'In progress',
  completed: 'Completed',
  cancelled: 'Cancelled',
}

export const TRIP_STATUS_STYLES: Record<TripStatus, string> = {
  draft: 'bg-slate-100 text-slate-700',
  recruiting: 'bg-sky-100 text-sky-800',
  in_progress: 'bg-amber-100 text-amber-900',
  completed: 'bg-emerald-100 text-emerald-800',
  cancelled: 'bg-rose-100 text-rose-800',
}

export const JOIN_STATUS_LABELS: Record<JoinRequestStatus, string> = {
  pending: 'Requested',
  approved: 'Approved',
  rejected: 'Rejected',
  cancelled: 'Withdrawn',
}

/** "4.8" or "—". A user with no ratings has null, not 0. */
export function formatRating(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : value.toFixed(1)
}

export function initials(fullName: string): string {
  return fullName
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase() ?? '')
    .join('')
}

/** Tailwind class merge for the handful of places a variant is conditional. */
export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(' ')
}
