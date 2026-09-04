/**
 * Transport modes.
 *
 * The trip service takes `transport` as free text up to 40 characters — there
 * is no allowlist in the domain — so this is a UI vocabulary, not a contract.
 * The picker offers these and accepts anything else typed in; `iconFor` falls
 * back to a neutral glyph so a value from another client, or from a future
 * build, still renders.
 */

export interface TransportMode {
  value: string
  label: string
}

export const TRANSPORT_MODES: TransportMode[] = [
  { value: 'walk', label: 'On foot' },
  { value: 'bus', label: 'Bus' },
  { value: 'train', label: 'Train' },
  { value: 'car', label: 'Car' },
  { value: 'bike', label: 'Bicycle' },
  { value: 'boat', label: 'Boat' },
  { value: 'plane', label: 'Plane' },
]

const PATHS: Record<string, string> = {
  walk: 'M13 4a2 2 0 1 1-4 0 2 2 0 0 1 4 0Zm-1.4 3.2-3 1.6a1 1 0 0 0-.5.7L7.4 13H5.5a1 1 0 1 0 0 2h2.6a1 1 0 0 0 1-.8l.4-2 1.6 1.5.6 4.4a1 1 0 1 0 2-.3l-.7-4.8a1 1 0 0 0-.3-.6l-1.8-1.7.5-2.2 1 1.4a1 1 0 0 0 .8.4h2.3a1 1 0 1 0 0-2h-1.8l-1.6-2.2a1.6 1.6 0 0 0-1.9-.5Z',
  bus: 'M5 3h14a2 2 0 0 1 2 2v10a2 2 0 0 1-1 1.7V19a1 1 0 0 1-1 1h-1a1 1 0 0 1-1-1v-1H7v1a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1v-2.3A2 2 0 0 1 3 15V5a2 2 0 0 1 2-2Zm0 3v5h14V6H5Zm1.5 7a1.2 1.2 0 1 0 0 2.4 1.2 1.2 0 0 0 0-2.4Zm11 0a1.2 1.2 0 1 0 0 2.4 1.2 1.2 0 0 0 0-2.4Z',
  train: 'M7 2h10a3 3 0 0 1 3 3v9a3 3 0 0 1-3 3l1.7 2.4a1 1 0 0 1-1.6 1.2L14.5 17h-5l-2.6 3.6a1 1 0 0 1-1.6-1.2L7 17a3 3 0 0 1-3-3V5a3 3 0 0 1 3-3Zm-1 4v4h12V6H6Zm2 6.8a1.2 1.2 0 1 0 0 2.4 1.2 1.2 0 0 0 0-2.4Zm8 0a1.2 1.2 0 1 0 0 2.4 1.2 1.2 0 0 0 0-2.4Z',
  car: 'M5.6 6.3A2 2 0 0 1 7.5 5h9a2 2 0 0 1 1.9 1.3L19.7 10H21a1 1 0 1 1 0 2h-.5v6a1 1 0 0 1-1 1h-1.5a1 1 0 0 1-1-1v-1h-10v1a1 1 0 0 1-1 1H4.5a1 1 0 0 1-1-1v-6H3a1 1 0 1 1 0-2h1.3l1.3-3.7ZM6.9 10h10.2l-.9-3H7.8l-.9 3ZM7 13a1.2 1.2 0 1 0 0 2.4A1.2 1.2 0 0 0 7 13Zm10 0a1.2 1.2 0 1 0 0 2.4 1.2 1.2 0 0 0 0-2.4Z',
  bike: 'M5.5 14a2.5 2.5 0 1 1 0 5 2.5 2.5 0 0 1 0-5Zm13 0a2.5 2.5 0 1 1 0 5 2.5 2.5 0 0 1 0-5Zm0-2a4.5 4.5 0 1 0 4.4 5.5 1 1 0 0 0-2-.4A2.5 2.5 0 1 1 18.5 14Zm-13 0a4.5 4.5 0 1 0 4.4 5.5 1 1 0 1 0-2-.4A2.5 2.5 0 1 1 5.5 14ZM14 4a1 1 0 0 1 1-1h2a1 1 0 1 1 0 2h-1.2l1 2H18a1 1 0 1 1 0 2h-1.4l-1.4 4.3a1 1 0 0 1-1.9-.6l.4-1.2H9.9l-.6 1.8a1 1 0 0 1-1.9-.6L9.3 8H8a1 1 0 0 1 0-2h5.3l-.3-.6A1 1 0 0 1 14 4Zm-1.6 4-.8 2.5h4l.8-2.5h-4Z',
  boat: 'M12 2a1 1 0 0 1 1 1v1.6l5.6 1.9a1 1 0 0 1 .7 1.2L18 13h1.4a1 1 0 0 1 .9 1.4l-1.2 2.9a3.5 3.5 0 0 1-4.6 1.9 3.5 3.5 0 0 1-2.5 0 3.5 3.5 0 0 1-2.5 0A3.5 3.5 0 0 1 4.9 17l-1.2-2.9A1 1 0 0 1 4.6 13H6L4.7 7.7A1 1 0 0 1 5.4 6.5L11 4.6V3a1 1 0 0 1 1-1Zm0 4.6L6.9 8.3 8 13h8l1.1-4.7L12 6.6Z',
  plane: 'M10.2 3.4a1.6 1.6 0 0 1 3 .8v5l7.3 3.7a1 1 0 0 1 .5.9v1.6a1 1 0 0 1-1.3 1L13.2 14v3l2.3 1.7a1 1 0 0 1 .4.8v1.1a1 1 0 0 1-1.3 1L12 20.8l-2.6.8a1 1 0 0 1-1.3-1v-1.1a1 1 0 0 1 .4-.8l2.3-1.7v-3l-6.5 2.4a1 1 0 0 1-1.3-1v-1.6a1 1 0 0 1 .5-.9L10.8 9V4.6c0-.4.1-.9.4-1.2Z',
}

const FALLBACK = 'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20Zm0 4a1.2 1.2 0 1 1 0 2.4A1.2 1.2 0 0 1 12 6Zm1 4.5v6a1 1 0 1 1-2 0v-6a1 1 0 1 1 2 0Z'

export function TransportIcon({
  mode,
  className = 'h-4 w-4',
}: {
  mode: string | null
  className?: string
}) {
  if (!mode) return null
  const key = mode.trim().toLowerCase()
  const path = PATHS[key] ?? FALLBACK
  const label = TRANSPORT_MODES.find((m) => m.value === key)?.label ?? mode

  return (
    <svg viewBox="0 0 24 24" fill="currentColor" role="img" aria-label={label} className={className}>
      <title>{label}</title>
      <path d={path} />
    </svg>
  )
}

export function transportLabel(mode: string | null): string | null {
  if (!mode) return null
  const key = mode.trim().toLowerCase()
  return TRANSPORT_MODES.find((m) => m.value === key)?.label ?? mode
}
