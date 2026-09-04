import type { ReactNode } from 'react'
import { cx } from '@/lib/format'

/**
 * The inline message a form or a mutation failed with.
 *
 * `role="alert"` so it is announced when it appears — a submit that silently
 * paints a red sentence somewhere on the page is a submit that looks like it
 * did nothing.
 */
export function Alert({
  tone = 'error',
  children,
  className,
}: {
  tone?: 'error' | 'warning' | 'info' | 'success'
  children: ReactNode
  className?: string
}) {
  const tones = {
    error: 'bg-rose-50 text-rose-800 ring-rose-200',
    warning: 'bg-amber-50 text-amber-900 ring-amber-200',
    info: 'bg-sky-50 text-sky-900 ring-sky-200',
    success: 'bg-emerald-50 text-emerald-900 ring-emerald-200',
  }
  return (
    <div
      role={tone === 'error' ? 'alert' : 'status'}
      className={cx('rounded-lg px-3.5 py-2.5 text-sm ring-1 ring-inset', tones[tone], className)}
    >
      {children}
    </div>
  )
}
