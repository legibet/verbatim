import { useEffect, useState } from "react"
import { getKey, setKey, useKeyVersion } from "@/api"
import { TooltipProvider } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"
import { Models } from "@/views/Models"
import { Providers } from "@/views/Providers"
import { Requests } from "@/views/Requests"
import { Usage } from "@/views/Usage"

// Views live in the URL hash so a request detail is linkable:
// #/requests, #/requests/123, #/usage, #/models, #/providers.

type View = "requests" | "usage" | "models" | "providers"
type Route = { view: View; id: number | null }

function parseHash(hash: string): Route {
  const [view, id] = hash.replace(/^#\/?/, "").split("/")
  if (view === "usage" || view === "models" || view === "providers") return { view, id: null }
  const n = Number(id)
  return { view: "requests", id: id && Number.isInteger(n) && n > 0 ? n : null }
}

function useRoute(): Route {
  const [route, setRoute] = useState(() => parseHash(location.hash))
  useEffect(() => {
    const on = () => setRoute(parseHash(location.hash))
    window.addEventListener("hashchange", on)
    return () => window.removeEventListener("hashchange", on)
  }, [])
  return route
}

const NAV: { view: View; label: string }[] = [
  { view: "requests", label: "Requests" },
  { view: "usage", label: "Usage" },
  { view: "models", label: "Models" },
  { view: "providers", label: "Providers" },
]

export function App() {
  const route = useRoute()
  useKeyVersion() // re-render the footer when the key changes
  const hasKey = getKey() !== null

  return (
    <TooltipProvider delayDuration={250}>
      <div className="flex h-dvh flex-col">
        <header className="shrink-0 border-b">
          <div className="mx-auto flex h-12 max-w-[1600px] items-center gap-8 px-6">
            <a href="#/requests" className="font-mono text-[13px] font-medium tracking-tight">
              verbatim
            </a>
            <nav className="flex h-full items-stretch gap-1" aria-label="Views">
              {NAV.map((n) => (
                <a
                  key={n.view}
                  href={`#/${n.view}`}
                  aria-current={route.view === n.view ? "page" : undefined}
                  className={cn(
                    "relative flex items-center px-2.5 text-muted-foreground outline-none focus-visible:text-foreground focus-visible:underline [@media(hover:hover)]:hover:text-foreground",
                    "aria-[current=page]:font-medium aria-[current=page]:text-foreground aria-[current=page]:after:absolute aria-[current=page]:after:inset-x-2.5 aria-[current=page]:after:-bottom-px aria-[current=page]:after:h-px aria-[current=page]:after:bg-foreground",
                  )}
                >
                  {n.label}
                </a>
              ))}
            </nav>
          </div>
        </header>

        <main className="min-h-0 flex-1">
          <div className={cn("mx-auto h-full max-w-[1600px] px-6", route.view !== "requests" && "hidden")}>
            <Requests active={route.view === "requests"} openId={route.id} />
          </div>
          {route.view === "usage" && (
            <div className="h-full overflow-y-auto">
              <div className="mx-auto max-w-[1600px] px-6">
                <Usage />
              </div>
            </div>
          )}
          {route.view === "models" && (
            <div className="h-full overflow-y-auto">
              <div className="mx-auto max-w-[1600px] px-6">
                <Models />
              </div>
            </div>
          )}
          {route.view === "providers" && (
            <div className="h-full overflow-y-auto">
              <div className="mx-auto max-w-[1600px] px-6">
                <Providers />
              </div>
            </div>
          )}
        </main>

        <footer className="shrink-0 border-t">
          <div className="mx-auto flex h-8 max-w-[1600px] items-center justify-between px-6 text-[11.5px] text-muted-foreground">
            <span>The gateway log and live state. Costs are configured list prices.</span>
            {hasKey && (
              <button
                type="button"
                className="cursor-pointer underline-offset-4 outline-none transition-[scale] duration-150 active:scale-[0.97] focus-visible:underline [@media(hover:hover)]:hover:text-foreground [@media(hover:hover)]:hover:underline"
                onClick={() => setKey(null)}
              >
                Forget API key
              </button>
            )}
          </div>
        </footer>
      </div>
    </TooltipProvider>
  )
}
