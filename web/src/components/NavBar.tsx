import { useState } from 'react'
import { Link, NavLink, useNavigate } from 'react-router-dom'

import { useAuth } from '@/auth/AuthContext'
import { Avatar } from '@/components/ui/Avatar'
import { Button } from '@/components/ui/Button'
import { cx } from '@/lib/format'

const LINKS = [
  { to: '/main', label: 'Find trips' },
  { to: '/my-trips', label: 'My trips' },
  { to: '/chat', label: 'Chat' },
  { to: '/ratings', label: 'Ratings' },
]

export function NavBar() {
  const { user, logout } = useAuth()
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)

  const signOut = async () => {
    await logout()
    navigate('/login', { replace: true })
  }

  return (
    <header className="sticky top-0 z-40 border-b border-slate-200 bg-white/90 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-7xl items-center gap-4 px-4">
        <Link to="/main" className="flex items-center gap-2 font-semibold text-slate-900">
          <span aria-hidden className="text-lg">🧭</span>
          TogetherGo
        </Link>

        <nav className="hidden items-center gap-1 sm:flex" aria-label="Main">
          {LINKS.map((link) => (
            <NavLink
              key={link.to}
              to={link.to}
              className={({ isActive }) =>
                cx(
                  'rounded-lg px-3 py-1.5 text-sm font-medium transition-colors',
                  isActive
                    ? 'bg-brand-50 text-brand-700'
                    : 'text-slate-600 hover:bg-slate-100 hover:text-slate-900',
                )
              }
            >
              {link.label}
            </NavLink>
          ))}
        </nav>

        <div className="ml-auto flex items-center gap-2">
          <Link to="/organize-trip" className="hidden sm:block">
            <Button size="sm">Organize trip</Button>
          </Link>

          <div className="relative">
            <button
              type="button"
              onClick={() => setOpen((value) => !value)}
              aria-expanded={open}
              aria-haspopup="menu"
              className="flex items-center gap-2 rounded-full p-0.5 hover:bg-slate-100"
            >
              <Avatar name={user?.full_name ?? null} photoUrl={user?.photo_url} size="sm" />
              <span className="sr-only">Account menu</span>
            </button>

            {open && (
              <>
                {/* A click-away layer rather than a document listener: it
                    cannot leak, and it cannot fire before the toggle above. */}
                <div className="fixed inset-0 z-40" onClick={() => setOpen(false)} aria-hidden />
                <div
                  role="menu"
                  className="absolute right-0 z-50 mt-2 w-52 animate-fade-in overflow-hidden rounded-xl border border-slate-200 bg-white shadow-lg"
                >
                  <div className="border-b border-slate-100 px-3 py-2.5">
                    <p className="truncate text-sm font-medium text-slate-900">
                      {user?.full_name}
                    </p>
                    <p className="truncate text-xs text-slate-500">{user?.email}</p>
                  </div>
                  <Link
                    to="/profile"
                    role="menuitem"
                    onClick={() => setOpen(false)}
                    className="block px-3 py-2 text-sm text-slate-700 hover:bg-slate-50"
                  >
                    Profile
                  </Link>
                  <Link
                    to="/organize-trip"
                    role="menuitem"
                    onClick={() => setOpen(false)}
                    className="block px-3 py-2 text-sm text-slate-700 hover:bg-slate-50 sm:hidden"
                  >
                    Organize trip
                  </Link>
                  <button
                    type="button"
                    role="menuitem"
                    onClick={signOut}
                    className="block w-full px-3 py-2 text-left text-sm text-rose-700 hover:bg-rose-50"
                  >
                    Log out
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      </div>

      <nav className="flex gap-1 overflow-x-auto border-t border-slate-200 px-2 py-1.5 sm:hidden" aria-label="Main">
        {LINKS.map((link) => (
          <NavLink
            key={link.to}
            to={link.to}
            className={({ isActive }) =>
              cx(
                'whitespace-nowrap rounded-lg px-3 py-1.5 text-sm font-medium',
                isActive ? 'bg-brand-50 text-brand-700' : 'text-slate-600',
              )
            }
          >
            {link.label}
          </NavLink>
        ))}
      </nav>
    </header>
  )
}
