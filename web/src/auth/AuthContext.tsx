import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { useQueryClient } from '@tanstack/react-query'

import * as authApi from '@/lib/api/auth'
import * as usersApi from '@/lib/api/users'
import { refreshAccessToken } from '@/lib/api/client'
import { clearTokens, hasRefresh, onSessionExpired } from '@/lib/api/tokens'
import type { LoginBody, PrivateProfile, RegisterBody } from '@/lib/types'

interface AuthContextValue {
  user: PrivateProfile | null
  /** True until the startup refresh has settled. Gates the first render. */
  isLoading: boolean
  login: (body: LoginBody) => Promise<void>
  register: (body: RegisterBody) => Promise<void>
  logout: () => Promise<void>
  /** Replaces the cached profile after an edit, without a refetch. */
  setUser: (user: PrivateProfile) => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<PrivateProfile | null>(null)
  const [isLoading, setIsLoading] = useState(true)
  const queryClient = useQueryClient()

  /**
   * Startup. The access token lives in memory, so a reload always begins with
   * no token and either a refresh cookie or a stored refresh token. Trying the
   * refresh once here is what turns that into a session instead of a bounce to
   * /login on every F5.
   */
  useEffect(() => {
    let cancelled = false

    const restore = async () => {
      if (!hasRefresh()) {
        if (!cancelled) setIsLoading(false)
        return
      }
      try {
        await refreshAccessToken()
        const profile = await usersApi.me()
        if (!cancelled) setUser(profile)
      } catch {
        // No session to restore. Not an error worth showing anyone: it is what
        // a first visit looks like.
        if (!cancelled) {
          clearTokens()
          setUser(null)
        }
      } finally {
        if (!cancelled) setIsLoading(false)
      }
    }

    void restore()
    return () => {
      cancelled = true
    }
  }, [])

  /**
   * A refresh that failed for good, from anywhere in the app. The client has
   * already cleared the tokens; this drops the user and the cached server
   * state so the next render is /login with nothing stale behind it.
   */
  useEffect(
    () =>
      onSessionExpired(() => {
        setUser(null)
        queryClient.clear()
      }),
    [queryClient],
  )

  const login = useCallback(async (body: LoginBody) => {
    const auth = await authApi.login(body)
    setUser(auth.user)
  }, [])

  const register = useCallback(async (body: RegisterBody) => {
    const auth = await authApi.register(body)
    setUser(auth.user)
  }, [])

  const logout = useCallback(async () => {
    await authApi.logout()
    setUser(null)
    // Every cached query was fetched as this user. Keeping any of it across a
    // sign-out is how the next person to use the browser sees the last one's
    // join requests.
    queryClient.clear()
  }, [queryClient])

  const value = useMemo(
    () => ({ user, isLoading, login, register, logout, setUser }),
    [user, isLoading, login, register, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext)
  if (!context) throw new Error('useAuth must be used inside <AuthProvider>')
  return context
}

/** The signed-in user, for the many screens that only render behind the guard. */
export function useCurrentUser(): PrivateProfile {
  const { user } = useAuth()
  if (!user) throw new Error('useCurrentUser used outside a guarded route')
  return user
}
