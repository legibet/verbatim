# verbatim web UI

Vite + React + TypeScript + Tailwind + shadcn/ui; `pnpm build` writes to `../internal/ui/dist`, which is committed and embedded by the Go binary.

- Install: `pnpm install`
- Dev: `pnpm dev` (proxies `/ui/api` to `http://127.0.0.1:8080`, override with `VERBATIM_URL`); open http://localhost:5173/ui/
- Build: `pnpm build`, then `go build ./cmd/verbatim`
