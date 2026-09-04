import type { ReactNode } from 'react'

/** The card /login and /registration share. */
export function AuthShell({
  title,
  subtitle,
  children,
  footer,
  wide = false,
}: {
  title: string
  subtitle?: string
  children: ReactNode
  footer?: ReactNode
  wide?: boolean
}) {
  return (
    <div className="flex min-h-full items-center justify-center bg-slate-50 px-4 py-12">
      <div className={wide ? 'w-full max-w-xl' : 'w-full max-w-sm'}>
        <div className="mb-8 text-center">
          <span aria-hidden className="text-3xl">🧭</span>
          <h1 className="mt-2 text-2xl font-semibold text-slate-900">TogetherGo</h1>
        </div>

        <div className="rounded-2xl border border-slate-200 bg-white p-6 shadow-sm sm:p-8">
          <h2 className="text-lg font-semibold text-slate-900">{title}</h2>
          {subtitle && <p className="mt-1 text-sm text-slate-500">{subtitle}</p>}
          <div className="mt-6">{children}</div>
        </div>

        {footer && <p className="mt-6 text-center text-sm text-slate-600">{footer}</p>}
      </div>
    </div>
  )
}
