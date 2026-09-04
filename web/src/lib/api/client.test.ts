import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api, refreshAccessToken } from './client'
import { ApiError } from './errors'
import { clearTokens, getAccessToken, onSessionExpired, setTokens } from './tokens'

/**
 * These cover the one cross-cutting rule that cannot be checked by reading the
 * code: that N concurrent 401s produce exactly one refresh.
 *
 * It matters more than it looks. Refresh tokens rotate on every use and reuse
 * of a rotated one revokes the whole chain (CLAUDE.md), so a burst that fires
 * six refreshes does not merely waste five requests — five of them present an
 * already-rotated token and log the user out.
 */

type Handler = (url: string, init: RequestInit | undefined) => Response | Promise<Response>

let handler: Handler
const calls: { url: string; init: RequestInit | undefined }[] = []

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function apiError(status: number, code: string): Response {
  return json({ error: { code, message: 'nope' } }, status)
}

beforeEach(() => {
  calls.length = 0
  clearTokens()
  vi.stubGlobal('fetch', (url: string, init: RequestInit | undefined) => {
    calls.push({ url: String(url), init })
    return handler(String(url), init)
  })
})

afterEach(() => {
  clearTokens()
})

const refreshCalls = () => calls.filter((call) => call.url.includes('/api/auth/refresh'))

describe('refresh on 401', () => {
  it('refreshes once for a burst of concurrent 401s and retries each request', async () => {
    setTokens({ access_token: 'stale', refresh_token: 'r1' })

    handler = (url, init) => {
      if (url.includes('/api/auth/refresh')) {
        return json({ access_token: 'fresh', refresh_token: 'r2', expires_in: 900 })
      }
      const auth = (init?.headers as Record<string, string> | undefined)?.Authorization
      // The whole point: only the fresh token is accepted, so a request that
      // succeeds is one that was retried after the refresh.
      if (auth === 'Bearer fresh') return json({ ok: url })
      return apiError(401, 'token_expired')
    }

    const results = await Promise.all([
      api.get<{ ok: string }>('/api/users/me'),
      api.get<{ ok: string }>('/api/trips'),
      api.get<{ ok: string }>('/api/trips/a'),
      api.get<{ ok: string }>('/api/trips/b'),
      api.get<{ ok: string }>('/api/trips/c'),
      api.get<{ ok: string }>('/api/trips/d'),
    ])

    expect(results).toHaveLength(6)
    expect(refreshCalls()).toHaveLength(1)
    expect(getAccessToken()).toBe('fresh')
  })

  it('retries once and no more — a second 401 is reported, not re-refreshed', async () => {
    setTokens({ access_token: 'stale', refresh_token: 'r1' })

    handler = (url) => {
      if (url.includes('/api/auth/refresh')) {
        return json({ access_token: 'also-stale', refresh_token: 'r2', expires_in: 900 })
      }
      // Never accepts anything: an endpoint that answers 401 for its own
      // reasons must not become an infinite refresh loop.
      return apiError(401, 'token_expired')
    }

    await expect(api.get('/api/users/me')).rejects.toBeInstanceOf(ApiError)
    expect(refreshCalls()).toHaveLength(1)
    expect(calls.filter((call) => call.url.includes('/api/users/me'))).toHaveLength(2)
  })

  it('clears the session and notifies once when the refresh itself fails', async () => {
    setTokens({ access_token: 'stale', refresh_token: 'r1' })

    handler = (url) => {
      if (url.includes('/api/auth/refresh')) return apiError(401, 'refresh_reused')
      return apiError(401, 'token_expired')
    }

    const expired = vi.fn()
    const unsubscribe = onSessionExpired(expired)

    const outcomes = await Promise.allSettled([
      api.get('/api/users/me'),
      api.get('/api/trips'),
      api.get('/api/trips/a'),
    ])

    expect(outcomes.every((outcome) => outcome.status === 'rejected')).toBe(true)
    expect(refreshCalls()).toHaveLength(1)
    expect(expired).toHaveBeenCalledTimes(1)
    expect(getAccessToken()).toBeNull()

    unsubscribe()
  })

  it('does not refresh a 401 from an anonymous call', async () => {
    handler = () => apiError(401, 'invalid_credentials')

    await expect(
      api.post('/api/auth/login', { email: 'a@b.c', password: 'x' }, { anonymous: true }),
    ).rejects.toMatchObject({ code: 'invalid_credentials' })

    expect(refreshCalls()).toHaveLength(0)
  })

  it('starts a new refresh after the previous one has settled', async () => {
    setTokens({ access_token: 'stale', refresh_token: 'r1' })
    let issued = 0

    handler = (url, init) => {
      if (url.includes('/api/auth/refresh')) {
        issued += 1
        return json({ access_token: `t${issued}`, refresh_token: `r${issued + 1}`, expires_in: 900 })
      }
      const auth = (init?.headers as Record<string, string> | undefined)?.Authorization
      return auth === `Bearer t${issued}` ? json({ ok: true }) : apiError(401, 'token_expired')
    }

    await api.get('/api/users/me')
    setTokens({ access_token: 'stale-again', refresh_token: 'r2' })
    await api.get('/api/users/me')

    // Two separate bursts, so two refreshes — the in-flight promise must be
    // cleared on settle, not cached for the life of the page.
    expect(refreshCalls()).toHaveLength(2)
  })

  it('does not attempt a refresh when there is no refresh token at all', async () => {
    handler = () => apiError(401, 'token_expired')

    await expect(api.get('/api/users/me')).rejects.toBeInstanceOf(ApiError)
    expect(refreshCalls()).toHaveLength(0)
  })
})

describe('refreshAccessToken', () => {
  it('hands the same promise to concurrent callers', async () => {
    setTokens({ access_token: 'stale', refresh_token: 'r1' })
    handler = () => json({ access_token: 'fresh', refresh_token: 'r2', expires_in: 900 })

    const [a, b, c] = await Promise.all([
      refreshAccessToken(),
      refreshAccessToken(),
      refreshAccessToken(),
    ])

    expect([a, b, c]).toEqual(['fresh', 'fresh', 'fresh'])
    expect(refreshCalls()).toHaveLength(1)
  })
})
