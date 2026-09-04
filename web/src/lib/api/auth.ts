import { api } from './client'
import { clearTokens, getRefreshToken, refreshTransport, setTokens } from './tokens'
import type { AuthResponse, LoginBody, RegisterBody } from '@/lib/types'

/** Both of these return a token pair, and both store it before resolving. */
export async function login(body: LoginBody): Promise<AuthResponse> {
  const auth = await api.post<AuthResponse>('/api/auth/login', body, { anonymous: true })
  setTokens(auth)
  return auth
}

export async function register(body: RegisterBody): Promise<AuthResponse> {
  const auth = await api.post<AuthResponse>('/api/auth/register', body, { anonymous: true })
  setTokens(auth)
  return auth
}

/**
 * Revokes the refresh chain server-side, then drops the local tokens.
 *
 * The local clear happens whatever the server said. A logout that leaves the
 * user signed in because the network blipped is the one failure mode this must
 * not have; the worst case of clearing anyway is a chain that stays valid until
 * it expires, and the token is gone from this browser either way.
 */
export async function logout(): Promise<void> {
  const refreshToken = getRefreshToken()
  try {
    await api.post<void>('/api/auth/logout', {
      refresh_token: refreshTransport === 'cookie' ? undefined : refreshToken,
    })
  } catch {
    // Already invalid, or unreachable. Nothing to recover.
  } finally {
    clearTokens()
  }
}
