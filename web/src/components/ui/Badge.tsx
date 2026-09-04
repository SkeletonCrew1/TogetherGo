import type { ReactNode } from 'react'
import { cx } from '@/lib/format'

export function Badge({
  children,
  className,
}: {
  children: ReactNode
  className?: string
}) {
  return (
    <span
      className={cx(
        'inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium',
        className ?? 'bg-slate-100 text-slate-700',
      )}
    >
      {children}
    </span>
  )
}
