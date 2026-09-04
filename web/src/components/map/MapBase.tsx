import { useEffect } from 'react'
import { MapContainer, TileLayer, useMap } from 'react-leaflet'
import type { LatLngBoundsExpression, Map as LeafletMap } from 'leaflet'

import { DEFAULT_CENTER, DEFAULT_ZOOM, OSM_ATTRIBUTION, OSM_TILE_URL } from './leafletSetup'
import type { ReactNode } from 'react'

/**
 * Recomputes the map's size after its container has been laid out.
 *
 * Leaflet measures the container once, on init. Any map that starts inside a
 * hidden tab, a `<dialog>`, or a column whose width settles a frame later ends
 * up with half its tiles missing until something resizes the window. The
 * timeout is deliberate: it runs after the browser has painted, which is the
 * point at which the container has its final size.
 */
function InvalidateOnMount({ deps = [] }: { deps?: unknown[] }) {
  const map = useMap()
  useEffect(() => {
    const id = setTimeout(() => map.invalidateSize(), 0)
    return () => clearTimeout(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [map, ...deps])
  return null
}

/** Fits the view to a set of points, once they exist. */
export function FitBounds({ bounds }: { bounds: LatLngBoundsExpression | null }) {
  const map = useMap()
  useEffect(() => {
    if (!bounds) return
    map.fitBounds(bounds, { padding: [32, 32], maxZoom: 14 })
  }, [map, bounds])
  return null
}

export function MapBase({
  center = DEFAULT_CENTER,
  zoom = DEFAULT_ZOOM,
  className = 'h-72 w-full',
  scrollWheelZoom = false,
  invalidateDeps,
  whenReady,
  children,
}: {
  center?: [number, number]
  zoom?: number
  className?: string
  /**
   * Off by default. A map that grabs the wheel is a map that traps the page
   * scroll on a phone; the ones that need it (the route editor) opt in.
   */
  scrollWheelZoom?: boolean
  invalidateDeps?: unknown[]
  whenReady?: (map: LeafletMap) => void
  children?: ReactNode
}) {
  return (
    <MapContainer
      center={center}
      zoom={zoom}
      scrollWheelZoom={scrollWheelZoom}
      className={`${className} z-0 rounded-xl`}
      ref={whenReady}
    >
      <TileLayer url={OSM_TILE_URL} attribution={OSM_ATTRIBUTION} />
      <InvalidateOnMount deps={invalidateDeps} />
      {children}
    </MapContainer>
  )
}
