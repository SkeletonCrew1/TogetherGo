import { useEffect, useState } from 'react'

/**
 * `value`, delayed.
 *
 * Used for the search box and the filter sidebar. Without it every keystroke is
 * a new query key and therefore a new request — and on this API a wasted one is
 * not free: full-text search runs `websearch_to_tsquery` and the radius filter
 * is a PostGIS predicate.
 */
export function useDebounced<T>(value: T, delayMs = 350): T {
  const [debounced, setDebounced] = useState(value)

  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(timer)
  }, [value, delayMs])

  return debounced
}
