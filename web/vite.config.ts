import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// The Go server serves the build at /ui/ (internal/ui embeds ../internal/ui/dist).
export default defineConfig({
  base: "/ui/",
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "src") },
  },
  build: {
    outDir: "../internal/ui/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/ui/api": process.env.VERBATIM_URL ?? "http://127.0.0.1:8080",
    },
  },
})
