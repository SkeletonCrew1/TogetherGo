import { Navigate, Outlet, useLocation } from 'react-router-dom'

import { useAuth } from './AuthContext'
import { FullPageSpinner } from '@/components/ui/Spinner'

/**
 * The gate on every screen but /login and /registration.
 *
 * It waits for `isLoading` rather than redirecting immediately, because on a
 * reload the answer to "is anyone signed in" takes one round trip. Redirecting
 * first and correcting later is the flash of /login that every SPA with an
 * in-memory token has until someone puts this check in.
 */
export function RequireAuth() {
  const { user, isLoading } = useAuth()
  const location = useLocation()

  if (isLoading) return <FullPageSpinner label="Restoring your session…" />

  if (!user) {
    // `state.from` is what sends them back where they were aiming after they
    // sign in — a shared link to a trip should survive the detour.
    return <Navigate to="/login" replace state={{ from: location }} />
  }

  return <Outlet />
}

/** The mirror image: /login and /registration when already signed in. */
export function RedirectIfAuthenticated() {
  const { user, isLoading } = useAuth()

  if (isLoading) return <FullPageSpinner label="Restoring your session…" />
  if (user) return <Navigate to="/main" replace />
  return <Outlet />
}
