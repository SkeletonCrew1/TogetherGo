import { Link } from 'react-router-dom'

import { Button } from '@/components/ui/Button'

export function NotFound() {
  return (
    <div className="flex min-h-full items-center justify-center px-4 py-16">
      <div className="text-center">
        <p className="text-sm font-medium text-brand-700">404</p>
        <h1 className="mt-2 text-2xl font-semibold text-slate-900">No such page</h1>
        <p className="mt-2 text-sm text-slate-600">
          The link may be old, or the trip may have been deleted.
        </p>
        <Link to="/main" className="mt-6 inline-block">
          <Button>Back to trips</Button>
        </Link>
      </div>
    </div>
  )
}
