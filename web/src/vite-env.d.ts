/// <reference types="vite/client" />

/**
 * The env vars this app reads. Declared rather than left to `vite/client`'s
 * index signature, so that a typo in `import.meta.env.VITE_...` is a build
 * error instead of `undefined` at runtime.
 */
interface ImportMetaEnv {
  readonly VITE_API_BASE_URL?: string
  readonly VITE_WS_BASE_URL?: string
  readonly VITE_REFRESH_TRANSPORT?: 'cookie' | 'body'
  readonly VITE_NOMINATIM_URL?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
