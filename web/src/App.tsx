import { Navigate, Route, Routes } from 'react-router-dom'

import { RedirectIfAuthenticated, RequireAuth } from '@/auth/RequireAuth'
import { Layout } from '@/components/Layout'
import { RouteErrorBoundary } from '@/components/RouteErrorBoundary'
import { Chat } from '@/routes/Chat'
import { Login } from '@/routes/Login'
import { Main } from '@/routes/Main'
import { MyTrips } from '@/routes/MyTrips'
import { NotFound } from '@/routes/NotFound'
import { OrganizeTrip } from '@/routes/OrganizeTrip'
import { Profile } from '@/routes/Profile'
import { Ratings } from '@/routes/Ratings'
import { Registration } from '@/routes/Registration'
import { TripDetail } from '@/routes/TripDetail'

/**
 * Every route is behind RequireAuth except the two auth screens — including
 * discovery, because the trip service requires a token on `GET /api/trips` as
 * well (internal/http/router.go puts the whole /api/trips group behind
 * `authenticate`). There is no anonymous browsing to build.
 */
export function App() {
  return (
    <Routes>
      <Route element={<RedirectIfAuthenticated />}>
        {/* The auth screens get their own boundaries: they render outside the
            Layout, which is where the others' boundary lives. */}
        <Route
          path="/login"
          element={
            <RouteErrorBoundary routeKey="login">
              <Login />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="/registration"
          element={
            <RouteErrorBoundary routeKey="registration">
              <Registration />
            </RouteErrorBoundary>
          }
        />
      </Route>

      <Route element={<RequireAuth />}>
        <Route element={<Layout />}>
          <Route path="/main" element={<Main />} />
          <Route path="/trip/:id" element={<TripDetail />} />
          <Route path="/organize-trip" element={<OrganizeTrip />} />
          <Route path="/my-trips" element={<MyTrips />} />
          <Route path="/profile" element={<Profile />} />
          <Route path="/chat" element={<Chat />} />
          <Route path="/chat/:tripId" element={<Chat />} />
          <Route path="/ratings" element={<Ratings />} />
        </Route>
      </Route>

      <Route path="/" element={<Navigate to="/main" replace />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  )
}
