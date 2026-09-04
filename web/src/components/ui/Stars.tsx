import { useId, useState } from 'react'
import { cx } from '@/lib/format'

function Star({ filled, className }: { filled: boolean; className?: string }) {
  return (
    <svg
      viewBox="0 0 20 20"
      aria-hidden
      className={cx('h-full w-full', filled ? 'text-amber-400' : 'text-slate-300', className)}
      fill="currentColor"
    >
      <path d="M10 1.6l2.6 5.3 5.8.8-4.2 4.1 1 5.8-5.2-2.8-5.2 2.8 1-5.8L1.6 7.7l5.8-.8L10 1.6z" />
    </svg>
  )
}

/** Read-only display: "4.8" beside four and a bit stars. */
export function RatingStars({
  value,
  count,
  size = 'md',
}: {
  value: number | null
  count?: number | null
  size?: 'sm' | 'md'
}) {
  const box = size === 'sm' ? 'h-3.5 w-3.5' : 'h-4 w-4'

  if (value === null) {
    return <span className="text-sm text-slate-400">No ratings yet</span>
  }

  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="flex gap-0.5" aria-hidden>
        {[1, 2, 3, 4, 5].map((star) => (
          <span key={star} className={box}>
            <Star filled={star <= Math.round(value)} />
          </span>
        ))}
      </span>
      <span className="text-sm font-medium text-slate-700">{value.toFixed(1)}</span>
      {count !== null && count !== undefined && (
        // "4.9 from one rating" and "4.9 from forty" are not the same
        // recommendation, which is why the count is shown wherever it is known.
        <span className="text-sm text-slate-400">({count})</span>
      )}
    </span>
  )
}

/**
 * The star input on /ratings.
 *
 * A radio group under the hood rather than five buttons: keyboard arrows move
 * between the options for free, the group has one tab stop, and "3 stars" is
 * what a screen reader reads out. `hover` only paints — the committed value is
 * whatever the radio says.
 */
export function StarInput({
  value,
  onChange,
  name,
  disabled = false,
}: {
  value: number | null
  onChange: (score: number) => void
  name?: string
  disabled?: boolean
}) {
  const generatedName = useId()
  const groupName = name ?? generatedName
  const [hovered, setHovered] = useState<number | null>(null)
  const shown = hovered ?? value ?? 0

  return (
    <div
      role="radiogroup"
      aria-label="Rating out of five"
      className="inline-flex items-center gap-1"
      onMouseLeave={() => setHovered(null)}
    >
      {[1, 2, 3, 4, 5].map((score) => (
        <label
          key={score}
          onMouseEnter={() => !disabled && setHovered(score)}
          className={cx(
            'h-7 w-7 rounded p-0.5',
            disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer',
            'focus-within:outline focus-within:outline-2 focus-within:outline-offset-1 focus-within:outline-brand-600',
          )}
        >
          <input
            type="radio"
            name={groupName}
            value={score}
            checked={value === score}
            disabled={disabled}
            onChange={() => onChange(score)}
            className="sr-only"
          />
          <span className="sr-only">{score === 1 ? '1 star' : `${score} stars`}</span>
          <Star filled={score <= shown} />
        </label>
      ))}
    </div>
  )
}
