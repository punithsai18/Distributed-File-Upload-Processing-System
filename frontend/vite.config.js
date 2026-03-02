import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    host: true,   // listen on all interfaces so other devices can reach the dev server
    proxy: {
      // Forward all API calls to the Go backend during development.
      // Without this, fetch('/upload') would hit the Vite dev server (port 5173)
      // instead of the Go server (port 8080), causing HTTP 404 errors.
      '^/(upload|status|files|health|download|delete)': 'http://localhost:8080',
    },
  },
})
