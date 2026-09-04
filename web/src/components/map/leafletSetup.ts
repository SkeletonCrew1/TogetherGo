import L from 'leaflet'
import 'leaflet/dist/leaflet.css'

/**
 * Leaflet's default marker is an image referenced by a relative URL from inside
 * the package, which Vite does not rewrite — the classic "markers are invisible
 * in production" bug. Every marker in this app is a `divIcon` built here
 * instead, so there is no image to lose and the numbered pins the route map
 * needs come for free.
 */

export const OSM_TILE_URL = 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png'
export const OSM_ATTRIBUTION =
  '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'

/** Roughly central Europe — a starting view, replaced as soon as there is data. */
export const DEFAULT_CENTER: [number, number] = [50.0614, 19.9366]
export const DEFAULT_ZOOM = 11

function pin(background: string, ring: string, content: string, size: number): L.DivIcon {
  return L.divIcon({
    className: '',
    html: `<span style="
      display:flex;align-items:center;justify-content:center;
      width:${size}px;height:${size}px;
      background:${background};color:#fff;
      border:2px solid ${ring};border-radius:9999px;
      box-shadow:0 1px 4px rgba(15,23,42,.45);
      font:600 ${Math.round(size * 0.48)}px/1 ui-sans-serif,system-ui,sans-serif;
    ">${content}</span>`,
    iconSize: [size, size],
    // Centre the icon on the coordinate rather than hanging it below: these are
    // circles, not teardrops, so their anchor is their middle.
    iconAnchor: [size / 2, size / 2],
    popupAnchor: [0, -size / 2],
  })
}

/** A numbered stop on a route. 1-based — `seq` is 0-based on the wire. */
export function numberedIcon(index: number, tone: 'start' | 'end' | 'mid' = 'mid'): L.DivIcon {
  const colors = {
    start: '#059669',
    end: '#e11d48',
    mid: '#1a5cf0',
  }
  return pin(colors[tone], '#ffffff', String(index), 30)
}

/** The single point the picker is placing. */
export function pickerIcon(): L.DivIcon {
  return pin('#1a5cf0', '#ffffff', '', 22)
}

export { L }
