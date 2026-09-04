import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Link } from 'react-router-dom'

import { ApiError, NetworkError } from '@/lib/api/errors'
import { Button } from '@/components/ui/Button'

interface Props {
  children: ReactNode
  /** Distinguishes one route's boundary from another in the reset key. */
  routeKey: string
}

interface State {
  error: Error | null
}

/**
 * One boundary per route (the brief's rule).
 *
 * Per route and not one at the root: a crash in /chat should not blank out the
 * navigation, and remounting a single screen is a recovery the user can drive
 * with a button. `routeKey` changing resets it, so navigating away from a
 * broken screen and back gives it a fresh try rather than showing the stale
 * error for the rest of the session.
 *
 * Note this catches *render* errors only. A failed query is not an exception —
 * it is a state each screen renders as an inline error, which is what lets it
 * offer a retry that refetches instead of a reload that loses everything.
 */
export class RouteErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidUpdate(previous: Props) {
    if (previous.routeKey !== this.props.routeKey && this.state.error) {
      this.setState({ error: null })
    }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // No error reporting service in the MVP, so the console is where this goes.
    // The component stack is the useful half and React does not include it in
    // the error itself.
    console.error('Unhandled error in route', this.props.routeKey, error, info.componentStack)
  }

  render() {
    const { error } = this.state
    if (!error) return this.props.children

    return (
      <div className="mx-auto max-w-lg px-4 py-16 text-center">
        <h1 className="text-lg font-semibold text-slate-900">This screen ran into a problem</h1>
        <p className="mt-2 text-sm text-slate-600">{describe(error)}</p>
        <div className="mt-6 flex justify-center gap-2">
          <Button onClick={() => this.setState({ error: null })}>Try again</Button>
          <Link to="/main">
            <Button variant="secondary">Back to trips</Button>
          </Link>
        </div>
        {import.meta.env.DEV && (
          <pre className="mt-6 overflow-x-auto rounded-lg bg-slate-900 p-3 text-left text-xs text-slate-200">
            {error.stack ?? error.message}
          </pre>
        )}
      </div>
    )
  }
}

function describe(error: Error): string {
  if (error instanceof NetworkError) return error.message
  if (error instanceof ApiError) return error.message
  return 'Something went wrong while rendering. Trying again may be enough.'
}
