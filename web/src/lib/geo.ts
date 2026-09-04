import type { Coordinates } from '@/lib/types'

/**
 * Six decimal places — about 11 cm at the equator, and the precision the brief
 * fixes for anything the map picker hands back.
 *
 * It matters beyond tidiness: a click gives ~13 digits of float noise, and
 * without rounding, two clicks on the same pixel produce two different points,
 * so "has the route changed" is always true and the form is never clean.
 */
export function round6(value: number): number {
  return Math.round(value * 1e6) / 1e6
}

export function roundCoordinates({ lat, lng }: Coordinates): Coordinates {
  return { lat: round6(lat), lng: round6(lng) }
}

export function formatCoordinates({ lat, lng }: Coordinates): string {
  return `${lat.toFixed(6)}, ${lng.toFixed(6)}`
}

const NOMINATIM_URL =
  import.meta.env.VITE_NOMINATIM_URL ?? 'https://nominatim.openstreetmap.org'

/**
 * Best-effort name for a coordinate.
 *
 * Returns null rather than throwing, and every caller shows the result in an
 * editable text field. That is the brief's rule and it is the right one:
 * Nominatim rate-limits the public instance hard, names a lake after the
 * nearest road often enough to matter, and answers in whatever language it
 * feels like. A guess in a field the user can fix is useful; a guess they
 * cannot fix is a wrong name on someone's trip.
 */
export async function reverseGeocode(
  { lat, lng }: Coordinates,
  signal?: AbortSignal,
): Promise<string | null> {
  const url = new URL('/reverse', NOMINATIM_URL)
  url.searchParams.set('format', 'jsonv2')
  url.searchParams.set('lat', String(lat))
  url.searchParams.set('lon', String(lng))
  url.searchParams.set('zoom', '16')

  try {
    const response = await fetch(url, { signal, headers: { Accept: 'application/json' } })
    if (!response.ok) return null
    const body = (await response.json()) as { name?: string; display_name?: string }
    if (body.name) return body.name
    if (!body.display_name) return null
    // display_name is the full postal address; the first component is the part
    // that reads like a place ("Kraków" out of "Kraków, Lesser Poland, Poland").
    return body.display_name.split(',')[0]?.trim() || null
  } catch {
    return null
  }
}

/** Great-circle distance in km. Used to label the radius circle. */
export function distanceKm(a: Coordinates, b: Coordinates): number {
  const R = 6371
  const toRad = (deg: number) => (deg * Math.PI) / 180
  const dLat = toRad(b.lat - a.lat)
  const dLng = toRad(b.lng - a.lng)
  const h =
    Math.sin(dLat / 2) ** 2 +
    Math.cos(toRad(a.lat)) * Math.cos(toRad(b.lat)) * Math.sin(dLng / 2) ** 2
  return 2 * R * Math.asin(Math.sqrt(h))
}
