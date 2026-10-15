import * as React from "react"
import { cn } from "@/lib/utils"

// A quiet tag for secondary attributes (provider type, operation).
function Badge({ className, ...props }: React.ComponentProps<"span">) {
  return (
    <span
      data-slot="badge"
      className={cn(
        "inline-flex w-fit shrink-0 items-center rounded-xs border border-border px-1.5 font-mono text-[11px] leading-[18px] whitespace-nowrap text-muted-foreground",
        className
      )}
      {...props}
    />
  )
}

export { Badge }
