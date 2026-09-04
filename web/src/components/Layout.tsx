import { Outlet, useLocation } from 'react-router-dom'

import { NavBar } from './NavBar'
import { RouteErrorBoundary } from './RouteErrorBoundary'

/**
 * The chrome every signed-in screen sits in.
 *
 * The boundary is inside the layout and keyed on the pathname: a screen that
 * throws loses the screen, not the navigation, and walking away from it resets
 * it.
 */
export function Layout() {
  const location = useLocation()

  return (
    <div className="flex min-h-full flex-col bg-slate-50">
      <NavBar />
      <main className="flex-1">
        <RouteErrorBoundary routeKey={location.pathname}>
          <Outlet />
        </RouteErrorBoundary>
      </main>
    </div>
  )
}
