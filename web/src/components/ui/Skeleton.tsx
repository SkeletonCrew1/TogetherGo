import { cx } from '@/lib/format'

/**
 * A grey block that stands in for content.
 *
 * `aria-hidden` throughout: the shapes mean nothing to a screen reader, and the
 * one thing it should hear — that something is loading — is announced once by
 * the region wrapper instead of eleven times by the bars inside it.
 */
export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={cx('animate-pulse rounded bg-slate-200', className)} />
}

export function SkeletonText({ lines = 3, className }: { lines?: number; className?: string }) {
  return (
    <div className={cx('space-y-2', className)}>
      {Array.from({ length: lines }, (_, index) => (
        <Skeleton
          key={index}
          className={cx('h-3.5', index === lines - 1 ? 'w-2/3' : 'w-full')}
        />
      ))}
    </div>
  )
}

/** Wraps a set of skeletons so the fact of loading is announced exactly once. */
export function LoadingRegion({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <div role="status" aria-live="polite" aria-busy="true">
      <span className="sr-only">{label}</span>
      {children}
    </div>
  )
}

export function TripCardSkeleton() {
  return (
    <div className="rounded-xl border border-slate-200 bg-white p-4">
      <div className="flex items-start justify-between gap-3">
        <Skeleton className="h-5 w-1/2" />
        <Skeleton className="h-5 w-16 rounded-full" />
      </div>
      <Skeleton className="mt-3 h-3.5 w-3/4" />
      <Skeleton className="mt-2 h-3.5 w-1/3" />
      <div className="mt-4 flex items-center gap-3">
        <Skeleton className="h-8 w-8 rounded-full" />
        <Skeleton className="h-3.5 w-24" />
        <Skeleton className="ml-auto h-3.5 w-16" />
      </div>
    </div>
  )
}
