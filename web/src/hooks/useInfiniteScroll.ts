import { useCallback, useRef } from 'react'

/**
 * A callback ref for a sentinel element: when it scrolls into view, `onReach`
 * fires.
 *
 * An IntersectionObserver rather than a scroll listener — no work on frames
 * where nothing crossed the threshold, and it is correct inside a scrolling
 * container without anyone computing offsets.
 *
 * Returned as a ref callback rather than taking a ref, because React calls it
 * with null on unmount and when the element changes, which is exactly where the
 * old observer has to be disconnected. `onReach` is held in a ref so that a new
 * closure on every render does not tear the observer down and rebuild it.
 *
 * `rootMargin` fetches a screenful early, so the next page is usually there
 * before the user reaches the bottom.
 */
export function useInfiniteScroll(onReach: () => void, enabled: boolean) {
  const callback = useRef(onReach)
  callback.current = onReach

  const observer = useRef<IntersectionObserver | null>(null)

  return useCallback(
    (node: HTMLElement | null) => {
      observer.current?.disconnect()
      observer.current = null
      if (!node || !enabled) return

      observer.current = new IntersectionObserver(
        (entries) => {
          if (entries.some((entry) => entry.isIntersecting)) callback.current()
        },
        { rootMargin: '400px 0px' },
      )
      observer.current.observe(node)
    },
    [enabled],
  )
}
