import { forwardRef, useId, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes, type TextareaHTMLAttributes } from 'react'
import { cx } from '@/lib/format'

const CONTROL =
  'block w-full rounded-lg border-0 bg-white px-3 py-2 text-slate-900 shadow-sm ring-1 ring-inset ' +
  'ring-slate-300 placeholder:text-slate-400 focus:ring-2 focus:ring-inset focus:ring-brand-600 ' +
  'disabled:bg-slate-50 disabled:text-slate-500 sm:text-sm'

const CONTROL_ERROR = 'ring-rose-400 focus:ring-rose-500'

interface LabelledProps {
  label: string
  error?: string
  hint?: ReactNode
  /** Hides the label visually but keeps it for assistive tech. */
  hideLabel?: boolean
}

/**
 * The wrapper the three controls share.
 *
 * The error is wired with `aria-describedby` and `aria-invalid` rather than
 * only coloured red, so the reason a submit failed is available to a screen
 * reader too — react-hook-form has the message either way, and this is the
 * only place that has to spend four lines on it.
 */
function Wrapper({
  label,
  error,
  hint,
  hideLabel,
  controlId,
  describedBy,
  children,
}: LabelledProps & {
  controlId: string
  describedBy: string | undefined
  children: ReactNode
}) {
  return (
    <div>
      <label
        htmlFor={controlId}
        className={cx(
          'block text-sm font-medium text-slate-800',
          hideLabel ? 'sr-only' : 'mb-1.5',
        )}
      >
        {label}
      </label>
      {children}
      {error ? (
        <p id={describedBy} role="alert" className="mt-1.5 text-sm text-rose-600">
          {error}
        </p>
      ) : hint ? (
        <p id={describedBy} className="mt-1.5 text-sm text-slate-500">
          {hint}
        </p>
      ) : null}
    </div>
  )
}

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement> & LabelledProps>(
  function Input({ label, error, hint, hideLabel, className, id, ...rest }, ref) {
    const generatedId = useId()
    const controlId = id ?? generatedId
    const describedBy = error || hint ? `${controlId}-desc` : undefined
    return (
      <Wrapper
        label={label}
        error={error}
        hint={hint}
        hideLabel={hideLabel}
        controlId={controlId}
        describedBy={describedBy}
      >
        <input
          ref={ref}
          id={controlId}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy}
          className={cx(CONTROL, error && CONTROL_ERROR, className)}
          {...rest}
        />
      </Wrapper>
    )
  },
)

export const Textarea = forwardRef<
  HTMLTextAreaElement,
  TextareaHTMLAttributes<HTMLTextAreaElement> & LabelledProps
>(function Textarea({ label, error, hint, hideLabel, className, id, ...rest }, ref) {
  const generatedId = useId()
  const controlId = id ?? generatedId
  const describedBy = error || hint ? `${controlId}-desc` : undefined
  return (
    <Wrapper
      label={label}
      error={error}
      hint={hint}
      hideLabel={hideLabel}
      controlId={controlId}
      describedBy={describedBy}
    >
      <textarea
        ref={ref}
        id={controlId}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        className={cx(CONTROL, error && CONTROL_ERROR, className)}
        {...rest}
      />
    </Wrapper>
  )
})

export const Select = forwardRef<
  HTMLSelectElement,
  SelectHTMLAttributes<HTMLSelectElement> & LabelledProps
>(function Select({ label, error, hint, hideLabel, className, id, children, ...rest }, ref) {
  const generatedId = useId()
  const controlId = id ?? generatedId
  const describedBy = error || hint ? `${controlId}-desc` : undefined
  return (
    <Wrapper
      label={label}
      error={error}
      hint={hint}
      hideLabel={hideLabel}
      controlId={controlId}
      describedBy={describedBy}
    >
      <select
        ref={ref}
        id={controlId}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        className={cx(CONTROL, 'pr-8', error && CONTROL_ERROR, className)}
        {...rest}
      >
        {children}
      </select>
    </Wrapper>
  )
})
