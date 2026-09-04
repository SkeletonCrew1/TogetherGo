import { describe, expect, it } from 'vitest'

import { buildUrl } from './client'
import { toSearchFilter } from '@/routes/main/Main'
import { EMPTY_FILTERS } from '@/routes/main/FilterSidebar'

/**
 * GET /api/trips validates its query string against a closed allowlist and
 * rejects a parameter it does not recognise — and, because an absent parameter
 * and an empty one are the same thing to it, `?min_days=` is a 400 rather than
 * an ignored filter. So the mapping from the sidebar's strings to the wire is
 * the thing standing between an untouched filter panel and a broken /main.
 */
describe('toSearchFilter', () => {
  it('sends nothing at all for an untouched panel', () => {
    expect(toSearchFilter(EMPTY_FILTERS, '')).toEqual({})
    expect(buildUrl('/api/trips', toSearchFilter(EMPTY_FILTERS, ''))).toBe('/api/trips')
  })

  it('omits blank numbers rather than sending NaN', () => {
    const filter = toSearchFilter(
      { ...EMPTY_FILTERS, minDays: '', maxDays: '', minFreeSlots: '' },
      '',
    )
    expect('min_days' in filter).toBe(false)
    expect('max_days' in filter).toBe(false)
    expect('min_free_slots' in filter).toBe(false)
  })

  it('keeps a zero free-slot filter, which is a real request', () => {
    // 0 is meaningful here — "show me full trips too" — and the naive
    // truthiness check would drop it.
    expect(toSearchFilter({ ...EMPTY_FILTERS, minFreeSlots: '0' }, '')).toMatchObject({
      min_free_slots: 0,
    })
  })

  it('only sends the radius trio together', () => {
    // radius_km alone is a validation error on the service: a radius with no
    // centre is not a filter.
    const withoutPoint = toSearchFilter({ ...EMPTY_FILTERS, radiusKm: 120 }, '')
    expect('radius_km' in withoutPoint).toBe(false)

    const withPoint = toSearchFilter(
      { ...EMPTY_FILTERS, radiusKm: 120, near: { name: 'Kraków', lat: 50.061389, lng: 19.936583 } },
      '',
    )
    expect(withPoint).toMatchObject({
      near_lat: 50.061389,
      near_lng: 19.936583,
      radius_km: 120,
    })
  })

  it('makes date_to inclusive of the day chosen', () => {
    const filter = toSearchFilter({ ...EMPTY_FILTERS, dateTo: '2026-09-15' }, '')
    // "to the 15th" has to include the 15th; midnight would exclude the whole
    // day the user picked.
    expect(new Date(filter.date_to as string).getHours()).not.toBe(0)
    expect(filter.date_to).toMatch(/Z$/)
  })

  it('trims the query and drops an empty one', () => {
    expect(toSearchFilter(EMPTY_FILTERS, '   ')).toEqual({})
    expect(toSearchFilter(EMPTY_FILTERS, '  tatras  ')).toEqual({ q: 'tatras' })
  })

  it('serialises categories as one comma-separated parameter', () => {
    // Repeating the key is an explicit error from the service: "must be given
    // at most once".
    const filter = toSearchFilter({ ...EMPTY_FILTERS, categories: ['hiking', 'food'] }, '')
    const url = buildUrl('/api/trips', filter)
    expect(url).toBe('/api/trips?categories=food%2Chiking')
    expect(url.match(/categories=/g)).toHaveLength(1)
  })
})
