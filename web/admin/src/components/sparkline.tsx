import { cn } from "@/lib/utils"

interface SparklineProps {
  points: number[]
  className?: string
  strokeClassName?: string
  /** Render a soft area fill under the line (10% opacity). */
  fill?: boolean
}

/** Dependency-free SVG sparkline. Stretches to its container; stroke stays crisp. */
export function Sparkline({ points, className, strokeClassName, fill = false }: SparklineProps) {
  if (points.length < 2) return null
  const min = Math.min(...points)
  const range = Math.max(...points) - min || 1
  const coords = points.map((p, i) => {
    const x = (i / (points.length - 1)) * 100
    const y = 31 - ((p - min) / range) * 30
    return `${x.toFixed(2)},${y.toFixed(2)}`
  })
  return (
    <svg
      viewBox="0 0 100 32"
      preserveAspectRatio="none"
      aria-hidden="true"
      className={cn("h-8 w-full", className)}
    >
      {fill && (
        <polygon
          points={`0,32 ${coords.join(" ")} 100,32`}
          className={cn("fill-chart-1 opacity-10", strokeClassName && "fill-current")}
        />
      )}
      <polyline
        points={coords.join(" ")}
        fill="none"
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
        className={cn("stroke-chart-1", strokeClassName)}
      />
    </svg>
  )
}
