/**
 * Where the two tokens live.
 *
 * The access token is in memory and nowhere else — never localStorage, never a
 * cookie the SPA can read. It is short-lived (900s) and losing it on reload
 * costs one refresh call.
 *
 * The refresh token depends on VITE_REFRESH_TRANSPORT:
 *
 *   cookie — identity sets an httpOnly cookie, this module stores nothing, and
 *            `hasRefresh()` is a guess: the browser will send the cookie if it
 *            has one, and the only way to find out is to try the refresh.
 *   body   — identity returns the token in JSON (what it does today), so the
 *            SPA has to hold it to survive a reload. sessionStorage rather
 *            than localStorage: per-tab, gone when the tab closes, and not
 *            shared with a second tab that may have rotated it already —
 *            reuse of a rotated token revokes the whole chain (CLAUDE.md).
 *
 * Neither choice is a good place for a bearer credential in a world with XSS.
 * The cookie transport is the one to be on, and this module exists so that
 * moving to it is an env var rather than a refactor.
 */

export type RefreshTransport = 'cookie' | 'body'

export const refreshTransport: RefreshTransport =
  import.meta.env.VITE_REFRESH_TRANSPORT === 'cookie' ? 'cookie' : 'body'

const REFRESH_KEY = 'togethergo.refresh'

let accessToken: string | null = null

/** Called when a refresh fails for good, so the app can drop to /login once. */
type ExpiryListener = () => void
const expiryListeners = new Set<ExpiryListener>()

export function getAccessToken(): string | null {
  return accessToken
}

export function setTokens(tokens: { access_token: string; refresh_token?: string }): void {
  accessToken = tokens.access_token
  if (refreshTransport === 'body' && tokens.refresh_token) {
    try {
      sessionStorage.setItem(REFRESH_KEY, tokens.refresh_token)
    } catch {
      // Private mode, or storage disabled. The session then lives exactly as
      // long as the tab stays loaded, which is degraded but not broken.
    }
  }
}

export function getRefreshToken(): string | null {
  if (refreshTransport === 'cookie') return null
  try {
    return sessionStorage.getItem(REFRESH_KEY)
  } catch {
    return null
  }
}

/**
 * Whether a refresh is worth attempting at all — the difference between "the
 * app is starting up and may have a session" and "there is definitely nobody
 * logged in", which is what decides whether /login flashes on first paint.
 */
export function hasRefresh(): boolean {
  return refreshTransport === 'cookie' ? true : getRefreshToken() !== null
}

export function clearTokens(): void {
  accessToken = null
  try {
    sessionStorage.removeItem(REFRESH_KEY)
  } catch {
    // Nothing to clear.
  }
}

export function onSessionExpired(listener: ExpiryListener): () => void {
  expiryListeners.add(listener)
  return () => expiryListeners.delete(listener)
}

export function notifySessionExpired(): void {
  for (const listener of expiryListeners) listener()
}
