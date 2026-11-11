import * as React from "react"
import { cn } from "@/lib/utils"
import { XIcon } from "lucide-react"
import { Dialog as SheetPrimitive } from "radix-ui"

// A right-hand detail panel. It opens without an overlay (modal={false} on
// the root) so the table behind it stays readable and clickable.

function Sheet({ ...props }: React.ComponentProps<typeof SheetPrimitive.Root>) {
  return <SheetPrimitive.Root data-slot="sheet" {...props} />
}

function SheetContent({
  className,
  children,
  ...props
}: React.ComponentProps<typeof SheetPrimitive.Content>) {
  return (
    <SheetPrimitive.Portal>
      <SheetPrimitive.Content
        data-slot="sheet-content"
        className={cn(
          "fixed inset-y-0 right-0 z-50 flex h-full w-[min(680px,100vw)] flex-col border-l bg-background shadow-[-12px_0_32px_-12px_rgb(0_0_0/0.18)] outline-none",
          "data-[state=open]:animate-in data-[state=open]:slide-in-from-right-8 data-[state=open]:fade-in-0 data-[state=open]:duration-200 data-[state=open]:ease-[cubic-bezier(0.16,1,0.3,1)]",
          "data-[state=closed]:animate-out data-[state=closed]:slide-out-to-right-4 data-[state=closed]:fade-out-0 data-[state=closed]:duration-150 data-[state=closed]:ease-in",
          "overscroll-contain motion-reduce:animate-none",
          className
        )}
        {...props}
      >
        {children}
        <SheetPrimitive.Close className="absolute top-3 right-3 inline-flex size-8 cursor-pointer items-center justify-center rounded-sm text-muted-foreground transition-[background-color,color,scale] duration-150 outline-none active:scale-[0.94] focus-visible:ring-2 focus-visible:ring-ring/60 [@media(hover:hover)]:hover:bg-muted [@media(hover:hover)]:hover:text-foreground">
          <XIcon className="size-4" />
          <span className="sr-only">Close</span>
        </SheetPrimitive.Close>
      </SheetPrimitive.Content>
    </SheetPrimitive.Portal>
  )
}

function SheetTitle({ className, ...props }: React.ComponentProps<typeof SheetPrimitive.Title>) {
  return <SheetPrimitive.Title data-slot="sheet-title" className={cn("font-medium text-foreground", className)} {...props} />
}

export { Sheet, SheetContent, SheetTitle }
