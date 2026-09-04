export function Spinner({ className = 'h-5 w-5' }: { className?: string }) {
  return (
    <span
      role="status"
      aria-label="Loading"
      className={`inline-block animate-spin rounded-full border-2 border-slate-300 border-t-brand-600 ${className}`}
    />
  )
}

export function FullPageSpinner({ label }: { label?: string }) {
  return (
    <div className="flex min-h-screen flex-col items-center justify-center gap-3">
      <Spinner className="h-8 w-8" />
      {label && <p className="text-sm text-slate-500">{label}</p>}
    </div>
  )
}
