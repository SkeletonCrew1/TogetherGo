import { useMemo, useRef, useState } from 'react'
import { Marker, Polyline, useMapEvents } from 'react-leaflet'
import type { LatLngBoundsExpression } from 'leaflet'

import { FitBounds, MapBase } from './MapBase'
import { numberedIcon } from './leafletSetup'
import { TRANSPORT_MODES, TransportIcon } from '@/components/trip/transport'
import { Input, Select } from '@/components/ui/Field'
import { cx } from '@/lib/format'
import { formatCoordinates, reverseGeocode, round6, roundCoordinates } from '@/lib/geo'
import { TRIP_LIMITS, type Coordinates } from '@/lib/types'

/**
 * A route point while it is being edited.
 *
 * `key` is a client-side identity, not the server's `id`: the points do not
 * have one until the trip is saved, and React needs a stable key that survives
 * reordering — using the array index would make the inputs of two swapped rows
 * trade values.
 */
export interface DraftPoint {
  key: string
  name: string
  lat: number
  lng: number
  /** A `datetime-local` value, or ''. Converted to UTC on submit. */
  arriveAt: string
  transport: string
}

export function newDraftPoint(point: Coordinates): DraftPoint {
  const { lat, lng } = roundCoordinates(point)
  return {
    key: `pt-${Date.now()}-${Math.random().toString(16).slice(2, 8)}`,
    name: '',
    lat,
    lng,
    arriveAt: '',
    transport: '',
  }
}

function ClickToAdd({ onAdd, disabled }: { onAdd: (point: Coordinates) => void; disabled: boolean }) {
  useMapEvents({
    click: (event) => {
      if (disabled) return
      onAdd({ lat: event.latlng.lat, lng: event.latlng.lng })
    },
  })
  return null
}

/**
 * The map-based point editor: click the map to append a stop, drag a row to
 * reorder, drag a marker to move one, and edit name/time/transport per point.
 *
 * Reordering is drag-and-drop on the list rather than on the map, because the
 * order of a route is not a property of where its stops are — a there-and-back
 * hike visits the same coordinates in a meaningful sequence, and dragging pins
 * around could never express that.
 */
export function RouteEditor({
  points,
  onChange,
  error,
}: {
  points: DraftPoint[]
  onChange: (points: DraftPoint[]) => void
  error?: string
}) {
  const [dragKey, setDragKey] = useState<string | null>(null)
  const [overKey, setOverKey] = useState<string | null>(null)
  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const geocoding = useRef(new Set<string>())

  /**
   * The current route, readable from an async callback.
   *
   * `points` is a prop, so a closure created in one render holds that render's
   * array. A reverse-geocode that resolves after a stop was added would write
   * back the array from before the add and delete it. Anything that mutates the
   * route from a callback reads through this ref instead.
   */
  const latest = useRef(points)
  latest.current = points

  const atCapacity = points.length >= TRIP_LIMITS.pointsMax

  const bounds = useMemo<LatLngBoundsExpression | null>(
    () => (points.length ? points.map((p) => [p.lat, p.lng] as [number, number]) : null),
    [points],
  )

  const line = useMemo(() => points.map((p) => [p.lat, p.lng] as [number, number]), [points])

  const patch = (key: string, changes: Partial<DraftPoint>) => {
    onChange(points.map((point) => (point.key === key ? { ...point, ...changes } : point)))
  }

  /**
   * Seeds a name from reverse geocoding, once per point.
   *
   * The guard set matters: without it a re-render while the request is in
   * flight fires a second one, and the public Nominatim instance answers a
   * burst with 429 rather than names.
   */
  const suggestName = (point: DraftPoint) => {
    if (geocoding.current.has(point.key)) return
    geocoding.current.add(point.key)
    void reverseGeocode(point).then((name) => {
      geocoding.current.delete(point.key)
      if (!name) return
      // Only fill a name the user has not typed into. Their words win over a
      // suggestion that arrived late.
      onChange(
        latest.current.map((candidate) =>
          candidate.key === point.key && !candidate.name ? { ...candidate, name } : candidate,
        ),
      )
    })
  }

  const addPoint = (coordinates: Coordinates) => {
    const point = newDraftPoint(coordinates)
    onChange([...latest.current, point])
    setSelectedKey(point.key)
    suggestName(point)
  }

  const removePoint = (key: string) => {
    onChange(points.filter((point) => point.key !== key))
    if (selectedKey === key) setSelectedKey(null)
  }

  const move = (fromKey: string, toKey: string) => {
    if (fromKey === toKey) return
    const from = points.findIndex((point) => point.key === fromKey)
    const to = points.findIndex((point) => point.key === toKey)
    if (from < 0 || to < 0) return
    const next = [...points]
    const [moved] = next.splice(from, 1)
    next.splice(to, 0, moved)
    onChange(next)
  }

  /** Keyboard reordering, so the route is not mouse-only. */
  const nudge = (key: string, direction: -1 | 1) => {
    const index = points.findIndex((point) => point.key === key)
    const target = index + direction
    if (index < 0 || target < 0 || target >= points.length) return
    const next = [...points]
    ;[next[index], next[target]] = [next[target], next[index]]
    onChange(next)
  }

  return (
    <div className="space-y-3">
      <div className="overflow-hidden rounded-xl ring-1 ring-slate-200">
        <MapBase className="h-72 w-full" scrollWheelZoom invalidateDeps={[points.length]}>
          <FitBounds bounds={points.length <= 1 ? bounds : null} />
          <ClickToAdd onAdd={addPoint} disabled={atCapacity} />
          {line.length > 1 && (
            <Polyline positions={line} pathOptions={{ color: '#1a5cf0', weight: 3, opacity: 0.7 }} />
          )}
          {points.map((point, index) => (
            <Marker
              key={point.key}
              position={[point.lat, point.lng]}
              icon={numberedIcon(
                index + 1,
                index === 0 ? 'start' : index === points.length - 1 ? 'end' : 'mid',
              )}
              draggable
              eventHandlers={{
                click: () => setSelectedKey(point.key),
                dragend: (event) => {
                  const { lat, lng } = event.target.getLatLng()
                  patch(point.key, { lat: round6(lat), lng: round6(lng) })
                },
              }}
              opacity={selectedKey && selectedKey !== point.key ? 0.7 : 1}
            />
          ))}
        </MapBase>
      </div>

      <p className="text-xs text-slate-500">
        {atCapacity
          ? `That is the maximum of ${TRIP_LIMITS.pointsMax} stops.`
          : 'Click the map to add a stop. Drag a pin to move it, or a row to reorder.'}{' '}
        <span className="text-slate-400">
          {points.length}/{TRIP_LIMITS.pointsMax} stops · at least {TRIP_LIMITS.pointsMin} needed
        </span>
      </p>

      {error && (
        <p role="alert" className="text-sm text-rose-600">
          {error}
        </p>
      )}

      <ol className="space-y-2">
        {points.map((point, index) => (
          <li
            key={point.key}
            draggable
            onDragStart={() => setDragKey(point.key)}
            onDragEnd={() => {
              setDragKey(null)
              setOverKey(null)
            }}
            onDragOver={(event) => {
              // Without preventDefault the drop is never allowed and the whole
              // gesture silently does nothing.
              event.preventDefault()
              setOverKey(point.key)
            }}
            onDrop={(event) => {
              event.preventDefault()
              if (dragKey) move(dragKey, point.key)
              setDragKey(null)
              setOverKey(null)
            }}
            onClick={() => setSelectedKey(point.key)}
            className={cx(
              'rounded-xl border bg-white p-3 transition-colors',
              overKey === point.key && dragKey !== point.key
                ? 'border-brand-400 ring-2 ring-brand-100'
                : 'border-slate-200',
              dragKey === point.key && 'opacity-50',
              selectedKey === point.key && 'ring-2 ring-brand-200',
            )}
          >
            <div className="flex items-start gap-3">
              <span
                aria-hidden
                className="mt-1 flex h-7 w-7 shrink-0 cursor-grab items-center justify-center rounded-full bg-brand-600 text-xs font-semibold text-white active:cursor-grabbing"
              >
                {index + 1}
              </span>

              <div className="grid flex-1 gap-3 sm:grid-cols-[2fr,1fr,1fr]">
                <Input
                  label={`Stop ${index + 1} name`}
                  hideLabel
                  placeholder="Name this stop"
                  maxLength={TRIP_LIMITS.pointNameMax}
                  value={point.name}
                  onChange={(event) => patch(point.key, { name: event.target.value })}
                />
                <Input
                  label={`Stop ${index + 1} time`}
                  hideLabel
                  type="datetime-local"
                  value={point.arriveAt}
                  onChange={(event) => patch(point.key, { arriveAt: event.target.value })}
                />
                <div className="flex items-center gap-2">
                  <Select
                    label={`Stop ${index + 1} transport`}
                    hideLabel
                    className="flex-1"
                    value={point.transport}
                    onChange={(event) => patch(point.key, { transport: event.target.value })}
                  >
                    <option value="">How you get there</option>
                    {TRANSPORT_MODES.map((mode) => (
                      <option key={mode.value} value={mode.value}>
                        {mode.label}
                      </option>
                    ))}
                  </Select>
                  <span className="text-slate-400">
                    <TransportIcon mode={point.transport || null} className="h-5 w-5" />
                  </span>
                </div>
              </div>

              <div className="flex shrink-0 flex-col items-center">
                <button
                  type="button"
                  aria-label={`Move stop ${index + 1} up`}
                  disabled={index === 0}
                  onClick={() => nudge(point.key, -1)}
                  className="rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-700 disabled:opacity-30"
                >
                  ▲
                </button>
                <button
                  type="button"
                  aria-label={`Move stop ${index + 1} down`}
                  disabled={index === points.length - 1}
                  onClick={() => nudge(point.key, 1)}
                  className="rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-700 disabled:opacity-30"
                >
                  ▼
                </button>
                <button
                  type="button"
                  aria-label={`Remove stop ${index + 1}`}
                  onClick={() => removePoint(point.key)}
                  className="rounded p-1 text-slate-400 hover:bg-rose-50 hover:text-rose-600"
                >
                  ✕
                </button>
              </div>
            </div>

            <p className="mt-2 pl-10 font-mono text-xs text-slate-400">
              {formatCoordinates(point)}
            </p>
          </li>
        ))}
      </ol>
    </div>
  )
}
