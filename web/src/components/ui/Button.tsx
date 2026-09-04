import { forwardRef, type ButtonHTMLAttributes } from 'react'
import { cx } from '@/lib/format'

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger'
type Size = 'sm' | 'md' | 'lg'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  isLoading?: boolean
}

const VARIANTS: Record<Variant, string> = {
  primary: 'bg-brand-600 text-white hover:bg-brand-700 focus-visible:outline-brand-600',
  secondary:
    'bg-white text-slate-800 ring-1 ring-inset ring-slate-300 hover:bg-slate-50 focus-visible:outline-slate-500',
  ghost: 'text-slate-700 hover:bg-slate-100 focus-visible:outline-slate-400',
  danger: 'bg-rose-600 text-white hover:bg-rose-700 focus-visible:outline-rose-600',
}

const SIZES: Record<Size, string> = {
  sm: 'px-2.5 py-1.5 text-sm',
  md: 'px-3.5 py-2 text-sm',
  lg: 'px-4 py-2.5 text-base',
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'primary', size = 'md', isLoading = false, className, children, disabled, ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      // A button that is busy must not be clickable again, and `disabled` alone
      // would not tell a screen reader why.
      disabled={disabled || isLoading}
      aria-busy={isLoading || undefined}
      className={cx(
        'inline-flex items-center justify-center gap-2 rounded-lg font-medium transition-colors',
        'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2',
        'disabled:cursor-not-allowed disabled:opacity-60',
        VARIANTS[variant],
        SIZES[size],
        className,
      )}
      {...rest}
    >
      {isLoading && (
        <span
          aria-hidden
          className="h-3.5 w-3.5 animate-spin rounded-full border-2 border-current border-t-transparent"
        />
      )}
      {children}
    </button>
  )
})
