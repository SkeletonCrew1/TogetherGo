import { defineConfig } from 'vitest/config'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    // jsdom for sessionStorage, which the token store uses under the `body`
    // refresh transport.
    environment: 'jsdom',
    // .tsx as well as .ts: the composer's send path is only meaningful as a
    // component test — "the submit handler writes to the socket and makes no
    // request" is a statement about what the form does, not about a function.
    include: ['src/**/*.test.{ts,tsx}'],
    restoreMocks: true,
  },
})
