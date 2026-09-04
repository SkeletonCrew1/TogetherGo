import type { ReactNode } from 'react'
import { cx } from '@/lib/format'

export interface Tab<T extends string> {
  id: T
  label: string
  badge?: ReactNode
}

/**
 * A tab strip whose selection lives in the URL, not in state.
 *
 * /my-trips is a screen people link to and reload; `?tab=participate` means a
 * reload lands on the tab they were on, and the back button steps between
 * tabs the way it looks like it should.
 */
export function Tabs<T extends string>({
  tabs,
  active,
  onChange,
}: {
  tabs: Tab<T>[]
  active: T
  onChange: (id: T) => void
}) {
  return (
    <div role="tablist" className="flex gap-1 border-b border-slate-200">
      {tabs.map((tab) => {
        const selected = tab.id === active
        return (
          <button
            key={tab.id}
            role="tab"
            type="button"
            aria-selected={selected}
            onClick={() => onChange(tab.id)}
            className={cx(
              'relative -mb-px inline-flex items-center gap-2 border-b-2 px-4 py-2.5 text-sm font-medium transition-colors',
              'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand-600',
              selected
                ? 'border-brand-600 text-brand-700'
                : 'border-transparent text-slate-500 hover:border-slate-300 hover:text-slate-700',
            )}
          >
            {tab.label}
            {tab.badge}
          </button>
        )
      })}
    </div>
  )
}
