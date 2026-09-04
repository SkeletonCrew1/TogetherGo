import { useState } from 'react'
import { cx } from '@/lib/format'
import { initials } from '@/lib/format'

/**
 * A photo, or the initials it falls back to.
 *
 * Two reasons the fallback matters: photo_url is nullable on every user shape,
 * and it is a URL the user typed themselves (identity validates that it is an
 * absolute http(s) URL, not that it is an image). A broken img is a broken
 * layout, so onError switches to the initials.
 */
export function Avatar({
  name,
  photoUrl,
  size = 'md',
  className,
}: {
  name: string | null
  photoUrl?: string | null
  size?: 'xs' | 'sm' | 'md' | 'lg' | 'xl'
  className?: string
}) {
  const [broken, setBroken] = useState(false)
  const sizes = {
    xs: 'h-6 w-6 text-[10px]',
    sm: 'h-8 w-8 text-xs',
    md: 'h-10 w-10 text-sm',
    lg: 'h-14 w-14 text-base',
    xl: 'h-24 w-24 text-2xl',
  }
  const label = name ?? 'Unknown traveller'

  if (photoUrl && !broken) {
    return (
      <img
        src={photoUrl}
        alt={label}
        onError={() => setBroken(true)}
        className={cx('shrink-0 rounded-full object-cover', sizes[size], className)}
      />
    )
  }

  return (
    <span
      aria-hidden
      title={label}
      className={cx(
        'flex shrink-0 select-none items-center justify-center rounded-full bg-brand-100 font-semibold text-brand-800',
        sizes[size],
        className,
      )}
    >
      {name ? initials(name) : '?'}
    </span>
  )
}
