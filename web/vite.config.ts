import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// The SPA is served by this dev server inside a container and reaches the
// browser through Traefik on http://localhost:8080 — the same origin as the
// API. There is no proxy here any more and there must not be one: the gateway
// owns the routing table (deploy/traefik/dynamic.yml), and a second copy of it
// in this file is the thing that drifts. Same-origin is also what makes an
// httpOnly refresh cookie usable, so switching identity over to one needs no
// change here.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    // 0.0.0.0, not the default localhost. Inside a container "localhost" is the
    // container's own loopback, so a dev server bound to it is unreachable from
    // Traefik and the page is a 502. The Compose command passes --host as well;
    // this is here so `npm run dev` outside Docker behaves identically.
    host: '0.0.0.0',
    port: 5173,
    // Fail rather than silently move to 5174: the Traefik service points at
    // web:5173 by name, and a dev server that quietly picked another port would
    // present as a gateway error with nothing wrong in either config.
    strictPort: true,
    hmr: {
      // The page is served from 8080; the HMR client is not. Without this it
      // derives its websocket URL from the page's host and its *own* port and
      // dials ws://localhost:5173, which nothing publishes. The failure is
      // silent — a dead socket in the console, edits that never arrive, and a
      // dev server that looks fine because the initial page load went through
      // the gateway just fine.
      clientPort: 8080,
    },
    watch: {
      // Docker Desktop on macOS moves the bind mount through a virtualisation
      // layer that does not forward inotify events reliably, so chokidar's
      // native watcher sees nothing and no edit ever triggers a reload. Polling
      // costs some idle CPU and is the only thing that works here.
      usePolling: true,
      interval: 300,
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
