import * as React from "react"
import { cn } from "@/lib/utils"

// Dense ledger table: hairline row rules, sticky header, numbers right-aligned
// by the caller (className="num text-right").

function Table({ className, ...props }: React.ComponentProps<"table">) {
  return <table data-slot="table" className={cn("w-full border-separate border-spacing-0 text-[12.5px]", className)} {...props} />
}

function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return <thead data-slot="table-header" className={cn("sticky top-0 z-10", className)} {...props} />
}

function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return <tbody data-slot="table-body" className={className} {...props} />
}

function TableFooter({ className, ...props }: React.ComponentProps<"tfoot">) {
  return <tfoot data-slot="table-footer" className={cn("font-medium [&_td]:border-t [&_td]:border-b-0 [&_td]:border-border", className)} {...props} />
}

function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return (
    <tr
      data-slot="table-row"
      className={cn(
        "[@media(hover:hover)]:hover:[&>td]:bg-surface data-[state=selected]:[&>td]:bg-muted",
        className
      )}
      {...props}
    />
  )
}

function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      data-slot="table-head"
      className={cn(
        "h-8 border-b border-border bg-surface px-2 text-left align-middle text-[11.5px] font-medium whitespace-nowrap text-muted-foreground first:pl-4 last:pr-4",
        className
      )}
      {...props}
    />
  )
}

function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return (
    <td
      data-slot="table-cell"
      className={cn("border-b border-rule px-2 py-2 align-baseline leading-5 whitespace-nowrap first:pl-4 last:pr-4", className)}
      {...props}
    />
  )
}

export { Table, TableHeader, TableBody, TableFooter, TableHead, TableRow, TableCell }
