import { useEffect, useRef, useState } from 'react'
import { Circle, Marker, useMapEvents } from 'react-leaflet'

import { MapBase } from './MapBase'
import { pickerIcon } from './leafletSetup'
import { Input } from '@/components/ui/Field'
import { formatCoordinates, reverseGeocode, roundCoordinates } from '@/lib/geo'
import type { Coordinates, Place } from '@/lib/types'

function ClickToPlace({ onPick }: { onPick: (point: Coordinates) => void }) {
  useMapEvents({
    click: (event) => onPick(roundCoordinates({ lat: event.latlng.lat, lng: event.latlng.lng })),
  })
  return null
}

/**
 * Pick one coordinate, name it, and hand back `{name, lat, lng}` with the
 * coordinates rounded to six decimal places.
 *
 * The name is a plain text input seeded from reverse geocoding rather than
 * bound to it. That is the brief's rule and it earns its keep: Nominatim
 * answers in whatever language it likes, rate-limits the public instance, and
 * regularly names a trailhead after the nearest B-road. Editable text keeps the
 * suggestion useful and the mistake fixable.
 */
export function MapPicker({
  value,
  onChange,
  label = 'Location',
  radiusKm,
  className,
  scrollWheelZoom = false,
}: {
  value: Place | null
  onChange: (place: Place | null) => void
  label?: string
  /** Draws a circle around the point. Used by the departure-radius filter. */
  radiusKm?: number
  className?: string
  scrollWheelZoom?: boolean
}) {
  const [geocoding, setGeocoding] = useState(false)
  // Tracks which coordinate the in-flight lookup belongs to, so a slow answer
  // for an old click cannot overwrite the name of a newer one.
  const lookupFor = useRef<string | null>(null)

  const pick = (point: Coordinates) => {
    // The name is cleared, not kept: reusing the previous point's name on a new
    // coordinate is the one outcome that produces a confidently wrong label.
    onChange({ ...point, name: '' })
  }

  useEffect(() => {
    if (!value || value.name) return

    const key = `${value.lat},${value.lng}`
    lookupFor.current = key
    const controller = new AbortController()
    setGeocoding(true)

    void reverseGeocode(value, controller.signal)
      .then((name) => {
        if (lookupFor.current !== key) return
        if (name) onChange({ ...value, name })
      })
      .finally(() => {
        if (lookupFor.current === key) setGeocoding(false)
      })

    return () => controller.abort()
    // Only the coordinate should retrigger a lookup. Including `onChange` would
    // rerun it on every parent render, and including `value.name` would rerun
    // it on every keystroke in the field below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value?.lat, value?.lng])

  return (
    <div className={className}>
      <MapBase
        center={value ? [value.lat, value.lng] : undefined}
        zoom={value ? 12 : undefined}
        className="h-56 w-full"
        scrollWheelZoom={scrollWheelZoom}
      >
        <ClickToPlace onPick={pick} />
        {value && (
          <>
            <Marker position={[value.lat, value.lng]} icon={pickerIcon()} />
            {radiusKm !== undefined && radiusKm > 0 && (
              <Circle
                center={[value.lat, value.lng]}
                radius={radiusKm * 1000}
                pathOptions={{ color: '#1a5cf0', fillOpacity: 0.08, weight: 1.5 }}
              />
            )}
          </>
        )}
      </MapBase>

      <p className="mt-1.5 text-xs text-slate-500">
        {value ? (
          <>
            Click the map to move the point ·{' '}
            <span className="font-mono">{formatCoordinates(value)}</span>
          </>
        ) : (
          'Click the map to place a point.'
        )}
      </p>

      {value && (
        <div className="mt-2 flex items-end gap-2">
          <Input
            label={label}
            className="flex-1"
            value={value.name}
            placeholder={geocoding ? 'Looking up a name…' : 'Name this place'}
            onChange={(event) => onChange({ ...value, name: event.target.value })}
            hint={geocoding ? 'Suggesting a name — you can overwrite it.' : undefined}
          />
          <button
            type="button"
            onClick={() => onChange(null)}
            className="mb-0.5 rounded-lg px-2.5 py-2 text-sm text-slate-500 hover:bg-slate-100 hover:text-slate-700"
          >
            Clear
          </button>
        </div>
      )}
    </div>
  )
}
