import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import { cn } from "@/lib/utils"

// Every button acknowledges the press with a small scale-down.
const buttonVariants = cva(
  "inline-flex shrink-0 cursor-pointer items-center justify-center gap-1.5 rounded-sm text-[12.5px] font-medium whitespace-nowrap transition-[background-color,color,border-color,scale] duration-150 ease-out outline-none select-none active:scale-[0.97] focus-visible:ring-2 focus-visible:ring-ring/60 disabled:pointer-events-none disabled:opacity-50 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-3.5",
  {
    variants: {
      variant: {
        default: "bg-primary text-primary-foreground [@media(hover:hover)]:hover:bg-primary/85",
        outline:
          "border border-input bg-background text-foreground [@media(hover:hover)]:hover:bg-muted",
        ghost: "text-muted-foreground [@media(hover:hover)]:hover:bg-muted [@media(hover:hover)]:hover:text-foreground",
      },
      size: {
        default: "h-8 px-3",
        sm: "h-7 px-2.5",
        "icon-sm": "size-7",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

function Button({
  className,
  variant = "default",
  size = "default",
  ...props
}: React.ComponentProps<"button"> & VariantProps<typeof buttonVariants>) {
  return (
    <button
      data-slot="button"
      data-variant={variant}
      data-size={size}
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { Button }
