import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// The SPA talks to the Traefik gateway, never to a service directly. In dev
// that is a proxy rather than a CORS grant: same-origin requests are what make
// an httpOnly refresh cookie usable at all, and switching identity over to one
// then needs no change here.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
      '/.well-known': { target: 'http://localhost:8080', changeOrigin: true },
      '/ws': { target: 'ws://localhost:8080', ws: true, changeOrigin: true },
    },
  },
  build: {
    // Leaflet and the query client are the two chunks worth splitting: the map
    // is dead weight on /login and /profile, which is most of a first visit.
    rollupOptions: {
      output: {
        manualChunks: {
          leaflet: ['leaflet', 'react-leaflet'],
          query: ['@tanstack/react-query'],
        },
      },
    },
  },
})
