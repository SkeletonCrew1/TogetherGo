import { ApiError, NetworkError, toApiError } from './errors'
import {
  clearTokens,
  getAccessToken,
  getRefreshToken,
  hasRefresh,
  notifySessionExpired,
  refreshTransport,
  setTokens,
} from './tokens'
import type { TokenPair } from '@/lib/types'

/**
 * Empty, so every call is a relative path against the page's own origin.
 *
 * The SPA and the API are served by the same Traefik gateway on
 * http://localhost:8080, so `/api/trips` is already the right URL and there is
 * no cross-origin request to grant. That is the fix for the CORS problem
 * rather than a workaround for it — and same-origin is also what an httpOnly
 * refresh cookie needs to be usable at all.
 *
 * Set VITE_API_BASE_URL only for a build served from somewhere the API is not.
 * Vite inlines it at build time (see web/Dockerfile), so it is a property of
 * the bundle, not something a running container can be repointed with.
 */
export const API_BASE = import.meta.env.VITE_API_BASE_URL ?? ''

/** Query values the caller may pass; undefined and null keys are dropped. */
export type QueryValue = string | number | boolean | string[] | undefined | null

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'
  body?: unknown
  query?: Record<string, QueryValue>
  signal?: AbortSignal
  /**
   * Skip the Authorization header and the refresh-on-401 dance. For the auth
   * endpoints themselves: a 401 from /api/auth/login means the password is
   * wrong, and refreshing and retrying it would turn one clear error into two
   * confusing ones.
   */
  anonymous?: boolean
}

export function buildUrl(path: string, query?: Record<string, QueryValue>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value === undefined || value === null || value === '') continue
    // The trip service takes list filters as one comma-separated parameter and
    // rejects a repeated key outright ("must be given at most once").
    search.set(key, Array.isArray(value) ? value.join(',') : String(value))
  }
  const qs = search.toString()
  return `${API_BASE}${path}${qs ? `?${qs}` : ''}`
}

/* --------------------------------------------------------------------------
 * The refresh.
 *
 * One in-flight refresh, shared. Six queries mounting on /main all get 401 in
 * the same tick when the access token has expired; each of them awaiting the
 * same promise means one call to /api/auth/refresh. Firing six would be worse
 * than wasteful — refresh tokens rotate on every use and reuse of a rotated
 * one revokes the whole chain (CLAUDE.md), so five of the six would log the
 * user out.
 * ------------------------------------------------------------------------ */

let inFlightRefresh: Promise<string> | null = null

/**
 * Returns a fresh access token, refreshing at most once per burst.
 *
 * The promise is stored before it is awaited and cleared in a `finally`, so a
 * caller arriving mid-flight joins the existing attempt and one arriving after
 * it settles starts a new one.
 */
export function refreshAccessToken(): Promise<string> {
  if (inFlightRefresh) return inFlightRefresh

  inFlightRefresh = (async () => {
    if (!hasRefresh()) {
      throw new ApiError(401, 'no_session', 'Your session has ended. Sign in again.')
    }

    const response = await fetchOrThrow(buildUrl('/api/auth/refresh'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      // Under the cookie transport the token is not ours to send; the browser
      // attaches it, and the body is an empty object because identity's
      // endpoint still expects JSON.
      body: JSON.stringify(
        refreshTransport === 'cookie' ? {} : { refresh_token: getRefreshToken() },
      ),
      credentials: 'include',
    })

    if (!response.ok) {
      throw await toApiError(response)
    }

    const pair = (await response.json()) as TokenPair
    setTokens(pair)
    return pair.access_token
  })()

  // Attached before the promise is handed out, so a rejected refresh clears
  // the session exactly once however many callers were waiting on it.
  inFlightRefresh
    .catch(() => {
      clearTokens()
      notifySessionExpired()
    })
    .finally(() => {
      inFlightRefresh = null
    })

  return inFlightRefresh
}

async function fetchOrThrow(url: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(url, init)
  } catch (cause) {
    // An aborted request is a cancelled query, not a failure to report.
    if (cause instanceof DOMException && cause.name === 'AbortError') throw cause
    throw new NetworkError()
  }
}

/**
 * One API call.
 *
 * On 401 it refreshes and retries **once**. Once and not "until it works": a
 * token that is rejected twice in a row is not an expiry, and retrying a
 * request whose 401 comes from something else — a revoked chain, a clock skew,
 * an endpoint that answers 401 for its own reasons — is an infinite loop with
 * a network call in it.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query, signal, anonymous = false } = options
  const url = buildUrl(path, query)

  const send = async (token: string | null): Promise<Response> => {
    const headers: Record<string, string> = {}
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (token) headers.Authorization = `Bearer ${token}`

    return fetchOrThrow(url, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
      // Needed for the refresh cookie once identity sets one, and harmless
      // before then.
      credentials: 'include',
    })
  }

  let response = await send(anonymous ? null : getAccessToken())

  if (response.status === 401 && !anonymous) {
    let token: string
    try {
      token = await refreshAccessToken()
    } catch {
      // The refresh already cleared the session and told the app. Report the
      // original 401 rather than the refresh's, because the caller asked about
      // its own request.
      throw await toApiError(response)
    }
    response = await send(token)
  }

  if (!response.ok) throw await toApiError(response)

  // 204, and a 200 with no body, are both real answers here — DELETE
  // /requests/me and the logout endpoint among them.
  if (response.status === 204) return undefined as T
  const text = await response.text()
  if (!text) return undefined as T
  return JSON.parse(text) as T
}

export const api = {
  get: <T>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'GET' }),
  post: <T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'POST', body }),
  patch: <T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'PATCH', body }),
  delete: <T>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'DELETE' }),
}
