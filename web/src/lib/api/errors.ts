import type { ApiErrorBody } from '@/lib/types'

/**
 * A response the API answered with an error envelope.
 *
 * `code` is the thing to branch on, never `message` — CLAUDE.md fixes that the
 * status carries the class and the code carries the specific reason, and the
 * message is prose that may be reworded at any time.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly details: Record<string, unknown>

  constructor(status: number, code: string, message: string, details: Record<string, unknown> = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
  }

  /** True for the codes that mean "your view of this is stale, refetch". */
  get isConflict(): boolean {
    return this.status === 409
  }

  get isNotFound(): boolean {
    return this.status === 404
  }

  get isUnauthorized(): boolean {
    return this.status === 401
  }

  /**
   * Field errors as react-hook-form wants them, for the validation shape both
   * services produce: `details.fields` is a list of `{field, message}`.
   *
   * Nothing here assumes the key exists. A 422 from FastAPI and a 400 from the
   * trip service put their fields in the same place, but a code that carries no
   * fields at all is normal and must not throw on the way to a toast.
   */
  fieldErrors(): { field: string; message: string }[] {
    const fields = this.details.fields
    if (!Array.isArray(fields)) return []
    return fields.flatMap((entry) => {
      if (typeof entry !== 'object' || entry === null) return []
      const { field, message } = entry as Record<string, unknown>
      if (typeof field !== 'string' || typeof message !== 'string') return []
      return [{ field, message }]
    })
  }
}

/** The network never answered — DNS, offline, gateway down, request aborted. */
export class NetworkError extends Error {
  constructor(message = 'Could not reach the server.') {
    super(message)
    this.name = 'NetworkError'
  }
}

/**
 * Builds an ApiError from a non-2xx response.
 *
 * A body that is not the error envelope is normal at the edges: Traefik answers
 * a route with no backend in its own format, and a 502 may carry HTML. Those
 * become a synthetic code rather than an exception during error handling, which
 * would replace a useful message with "Unexpected token < in JSON".
 */
export async function toApiError(response: Response): Promise<ApiError> {
  let body: unknown = null
  try {
    body = await response.json()
  } catch {
    // Fall through to the synthetic envelope below.
  }

  const envelope = (body as ApiErrorBody | null)?.error
  if (envelope && typeof envelope.code === 'string') {
    return new ApiError(
      response.status,
      envelope.code,
      envelope.message || 'The request failed.',
      envelope.details ?? {},
    )
  }

  return new ApiError(
    response.status,
    `http_${response.status}`,
    response.status >= 500
      ? 'The server is having trouble. Try again in a moment.'
      : 'The request failed.',
  )
}

/** One sentence to show a user, for anything that can be thrown at them. */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof NetworkError) return error.message
  if (error instanceof Error && error.message) return error.message
  return 'Something went wrong.'
}
