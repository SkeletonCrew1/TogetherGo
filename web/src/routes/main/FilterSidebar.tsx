import { useState } from 'react'

import { MapPicker } from '@/components/map/MapPicker'
import { Button } from '@/components/ui/Button'
import { Input } from '@/components/ui/Field'
import { CATEGORY_LABELS, cx } from '@/lib/format'
import {
  CATEGORIES,
  SEARCH_LIMITS,
  TRIP_LIMITS,
  type Category,
  type Place,
} from '@/lib/types'

/**
 * The filter state as the sidebar holds it — strings, because that is what the
 * controls produce, and an empty string means "not filtering on this".
 * Converting to the API's numbers and timestamps happens in exactly one place
 * (`toSearchFilter` in Main.tsx), so there is no path that sends `min_days=NaN`.
 */
export interface FilterState {
  dateFrom: string
  dateTo: string
  near: Place | null
  radiusKm: number
  minDays: string
  maxDays: string
  minFreeSlots: string
  categories: Category[]
}

export const EMPTY_FILTERS: FilterState = {
  dateFrom: '',
  dateTo: '',
  near: null,
  radiusKm: 50,
  minDays: '',
  maxDays: '',
  minFreeSlots: '',
  categories: [],
}

/** How many filters are actually narrowing anything, for the badge and reset. */
export function activeFilterCount(state: FilterState): number {
  let count = 0
  if (state.dateFrom) count += 1
  if (state.dateTo) count += 1
  if (state.near) count += 1
  if (state.minDays) count += 1
  if (state.maxDays) count += 1
  if (state.minFreeSlots) count += 1
  return count + state.categories.length
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="px-4 py-4">
      <h3 className="mb-3 text-xs font-semibold uppercase tracking-wide text-slate-500">
        {title}
      </h3>
      {children}
    </section>
  )
}

export function FilterSidebar({
  value,
  onChange,
  onReset,
}: {
  value: FilterState
  onChange: (next: FilterState) => void
  onReset: () => void
}) {
  const [mapOpen, setMapOpen] = useState(false)

  const set = <K extends keyof FilterState>(key: K, next: FilterState[K]) =>
    onChange({ ...value, [key]: next })

  const toggleCategory = (category: Category) =>
    set(
      'categories',
      value.categories.includes(category)
        ? value.categories.filter((existing) => existing !== category)
        : [...value.categories, category],
    )

  const active = activeFilterCount(value)

  return (
    <div className="divide-y divide-slate-200 rounded-xl border border-slate-200 bg-white">
      <div className="flex items-center justify-between px-4 py-3">
        <h2 className="text-sm font-semibold text-slate-900">
          Filters
          {active > 0 && <span className="ml-1.5 font-normal text-slate-400">({active})</span>}
        </h2>
        {active > 0 && (
          <button
            type="button"
            onClick={onReset}
            className="text-sm text-brand-700 hover:underline"
          >
            Clear all
          </button>
        )}
      </div>

      <Section title="Dates">
        <div className="space-y-3">
          <Input
            label="From"
            type="date"
            value={value.dateFrom}
            onChange={(event) => set('dateFrom', event.target.value)}
          />
          <Input
            label="To"
            type="date"
            // A range whose end precedes its start is a 400 from the service.
            // The control refuses to produce one in the first place.
            min={value.dateFrom || undefined}
            value={value.dateTo}
            onChange={(event) => set('dateTo', event.target.value)}
          />
        </div>
      </Section>

      <Section title="Departure area">
        {value.near || mapOpen ? (
          <div className="space-y-3">
            <label className="block">
              <span className="mb-1.5 block text-sm font-medium text-slate-800">
                Radius: {value.radiusKm} km
              </span>
              <input
                type="range"
                min={SEARCH_LIMITS.radiusMinKm}
                max={SEARCH_LIMITS.radiusMaxKm}
                step={1}
                value={value.radiusKm}
                onChange={(event) => set('radiusKm', Number(event.target.value))}
                className="w-full accent-brand-600"
              />
              <span className="mt-1 flex justify-between text-xs text-slate-400">
                <span>{SEARCH_LIMITS.radiusMinKm} km</span>
                <span>{SEARCH_LIMITS.radiusMaxKm} km</span>
              </span>
            </label>

            <MapPicker
              value={value.near}
              onChange={(place) => set('near', place)}
              label="Departure point"
              radiusKm={value.radiusKm}
            />
          </div>
        ) : (
          <Button variant="secondary" size="sm" onClick={() => setMapOpen(true)}>
            Pick a point on the map
          </Button>
        )}
      </Section>

      <Section title="Duration">
        <div className="grid grid-cols-2 gap-3">
          <Input
            label="Min days"
            type="number"
            min={1}
            max={TRIP_LIMITS.maxDurationDays}
            placeholder="Any"
            value={value.minDays}
            onChange={(event) => set('minDays', event.target.value)}
          />
          <Input
            label="Max days"
            type="number"
            min={value.minDays || 1}
            max={TRIP_LIMITS.maxDurationDays}
            placeholder="Any"
            value={value.maxDays}
            onChange={(event) => set('maxDays', event.target.value)}
          />
        </div>
      </Section>

      <Section title="Free slots">
        <Input
          label="At least this many free places"
          hideLabel
          type="number"
          min={0}
          max={TRIP_LIMITS.capacityMax}
          placeholder="Any number of free places"
          value={value.minFreeSlots}
          onChange={(event) => set('minFreeSlots', event.target.value)}
        />
      </Section>

      <Section title="Categories">
        <div className="flex flex-wrap gap-2">
          {CATEGORIES.map((category) => {
            const selected = value.categories.includes(category)
            return (
              <button
                key={category}
                type="button"
                aria-pressed={selected}
                onClick={() => toggleCategory(category)}
                className={cx(
                  'rounded-full px-3 py-1.5 text-sm font-medium transition-colors',
                  'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand-600',
                  selected
                    ? 'bg-brand-600 text-white'
                    : 'bg-slate-100 text-slate-700 hover:bg-slate-200',
                )}
              >
                {CATEGORY_LABELS[category]}
              </button>
            )
          })}
        </div>
      </Section>
    </div>
  )
}
