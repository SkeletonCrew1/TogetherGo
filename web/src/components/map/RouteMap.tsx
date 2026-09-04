import { useMemo } from 'react'
import { Marker, Polyline, Popup } from 'react-leaflet'
import type { LatLngBoundsExpression } from 'leaflet'

import { FitBounds, MapBase } from './MapBase'
import { numberedIcon } from './leafletSetup'
import { formatTime } from '@/lib/format'
import type { RoutePoint } from '@/lib/types'

/**
 * A route as numbered markers joined by a polyline.
 *
 * The line is a straight join between consecutive stops, not a routed path.
 * That is honest: the service stores stops, not roads, and drawing a plausible
 * driving route between two of them would be inventing a claim about the trip
 * that its organizer never made.
 */
export function RouteMap({
  points,
  className = 'h-80 w-full',
  onSelect,
  selectedId,
}: {
  points: RoutePoint[]
  className?: string
  onSelect?: (pointId: string) => void
  selectedId?: string | null
}) {
  const ordered = useMemo(() => [...points].sort((a, b) => a.seq - b.seq), [points])

  const bounds = useMemo<LatLngBoundsExpression | null>(() => {
    if (ordered.length === 0) return null
    return ordered.map((point) => [point.lat, point.lng] as [number, number])
  }, [ordered])

  const line = useMemo(
    () => ordered.map((point) => [point.lat, point.lng] as [number, number]),
    [ordered],
  )

  return (
    <MapBase className={className} invalidateDeps={[ordered.length]}>
      <FitBounds bounds={bounds} />
      {line.length > 1 && (
        <Polyline positions={line} pathOptions={{ color: '#1a5cf0', weight: 3, opacity: 0.75 }} />
      )}
      {ordered.map((point, index) => (
        <Marker
          key={point.id}
          position={[point.lat, point.lng]}
          icon={numberedIcon(
            index + 1,
            index === 0 ? 'start' : index === ordered.length - 1 ? 'end' : 'mid',
          )}
          eventHandlers={onSelect ? { click: () => onSelect(point.id) } : undefined}
          opacity={selectedId && selectedId !== point.id ? 0.65 : 1}
        >
          <Popup>
            <span className="font-medium">{point.name}</span>
            {point.arrive_at && (
              <span className="block text-slate-500">{formatTime(point.arrive_at)}</span>
            )}
            {point.transport && (
              <span className="block text-slate-500">by {point.transport}</span>
            )}
          </Popup>
        </Marker>
      ))}
    </MapBase>
  )
}
