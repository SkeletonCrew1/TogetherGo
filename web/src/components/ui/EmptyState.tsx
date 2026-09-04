import type { ReactNode } from 'react'

export function EmptyState({
  title,
  description,
  action,
  icon,
}: {
  title: string
  description?: ReactNode
  action?: ReactNode
  icon?: ReactNode
}) {
  return (
    <div className="rounded-xl border border-dashed border-slate-300 bg-white/60 px-6 py-12 text-center">
      {icon && <div className="mb-3 flex justify-center text-slate-400">{icon}</div>}
      <h3 className="text-base font-semibold text-slate-800">{title}</h3>
      {description && (
        <div className="mx-auto mt-1.5 max-w-md text-sm text-slate-500">{description}</div>
      )}
      {action && <div className="mt-5 flex justify-center">{action}</div>}
    </div>
  )
}

/**
 * The empty state for a screen whose backend is not deployed.
 *
 * Distinct from "you have nothing here" on purpose: "no messages yet" and "the
 * chat service is not running" call for completely different actions, and a
 * screen that renders the first when the truth is the second sends whoever is
 * testing it looking for a bug in the SPA.
 */
export function ServiceUnavailableState({
  service,
  what,
  reference,
}: {
  service: string
  what: string
  reference: string
}) {
  return (
    <EmptyState
      title={`The ${service} service is not available`}
      description={
        <>
          <p>{what}</p>
          <p className="mt-2 text-slate-400">
            This screen is built and will work as soon as the endpoints answer. See{' '}
            <code className="rounded bg-slate-100 px-1 py-0.5 text-xs text-slate-600">
              {reference}
            </code>
            .
          </p>
        </>
      }
    />
  )
}
