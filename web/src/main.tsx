import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import { App } from './App'
import { AuthProvider } from '@/auth/AuthContext'
import { ApiError } from '@/lib/api/errors'
import './index.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The API client already refreshes and retries a 401 once. Retrying a
      // 4xx on top of that would repeat requests that were correctly refused —
      // a 404 for a trip that does not exist is not going to become a 200.
      retry: (failureCount, error) => {
        if (error instanceof ApiError && error.status < 500) return false
        return failureCount < 2
      },
      staleTime: 30_000,
      refetchOnWindowFocus: false,
    },
    mutations: {
      retry: false,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        {/* AuthProvider is inside the router because signing out navigates,
            and inside the query client because a dead session clears the
            cache. */}
        <AuthProvider>
          <App />
        </AuthProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
