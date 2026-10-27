import { useMemo } from "react"

import { Card, CardContent } from "@/components/ui/card"
import type { Measurement, Status } from "@/lib/api"
import { fmtShort, relative, sameMonth } from "@/lib/dates"
import { cn } from "@/lib/utils"

function Sparkline({ points }: { points: { t: number; w: number }[] }) {
  if (points.length < 2) return <div className="h-10" />
  const w = 160
  const h = 40
  const ts = points.map((p) => p.t)
  const ws = points.map((p) => p.w)
  const [t0, t1] = [Math.min(...ts), Math.max(...ts)]
  const [w0, w1] = [Math.min(...ws), Math.max(...ws)]
  const x = (t: number) => (t1 === t0 ? w / 2 : ((t - t0) / (t1 - t0)) * (w - 4) + 2)
  const y = (v: number) => (w1 === w0 ? h / 2 : h - 3 - ((v - w0) / (w1 - w0)) * (h - 6))
  const d = points.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.w).toFixed(1)}`).join(" ")
  const last = points[points.length - 1]
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="h-10 w-full" preserveAspectRatio="none" aria-hidden>
      <path d={d} fill="none" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" vectorEffect="non-scaling-stroke" />
      <circle cx={x(last.t)} cy={y(last.w)} r="3" fill="currentColor" />
    </svg>
  )
}

export function Summary({ month, readings, status }: { month: Date; readings: Measurement[]; status: Status | null }) {
  const stats = useMemo(() => {
    const inMonth = readings.filter((r) => r.valid && sameMonth(new Date(r.time), month))
    if (!inMonth.length) return null
    const ws = inMonth.map((r) => r.weight_kg)
    return {
      avg: ws.reduce((a, b) => a + b, 0) / ws.length,
      change: ws[ws.length - 1] - ws[0],
      count: ws.length,
      min: Math.min(...ws),
      max: Math.max(...ws),
      points: inMonth.map((r) => ({ t: new Date(r.time).getTime(), w: r.weight_kg })),
    }
  }, [month, readings])

  const latest = status?.latest
  const awake = status?.scale.awake
  const lastSeen = status?.scale.last_seen ? new Date(status.scale.last_seen) : null

  return (
    <Card className="gap-3 py-4">
      <CardContent className="flex flex-col gap-3 px-4">
        <div className="flex items-start justify-between gap-3">
          <div>
            <div className="text-xs text-muted-foreground">Latest</div>
            {latest ? (
              <div className="flex items-baseline gap-1.5">
                <span className="text-4xl font-semibold tracking-tight tabular-nums">{latest.weight_kg.toFixed(1)}</span>
                <span className="text-muted-foreground">kg</span>
              </div>
            ) : (
              <div className="text-2xl font-semibold text-muted-foreground">—</div>
            )}
            {latest && (
              <div className="text-xs text-muted-foreground">
                {fmtShort(new Date(latest.time))}
                {latest.bmi ? ` · BMI ${latest.bmi.toFixed(1)}` : ""}
                {latest.composition ? ` · fat ${latest.composition.body_fat_percent.toFixed(1)}%` : ""}
              </div>
            )}
          </div>
          <div
            className="flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs text-muted-foreground"
            title={lastSeen ? `Last seen ${lastSeen.toLocaleString()}` : undefined}
          >
            <span
              className={cn(
                "size-2 rounded-full",
                !status?.daemon.running ? "bg-muted-foreground/40" : awake ? "animate-pulse bg-emerald-500" : "bg-muted-foreground/60",
              )}
            />
            {!status ? "…" : !status.daemon.running ? "offline" : awake ? "scale awake" : lastSeen ? `seen ${relative(lastSeen)}` : "asleep"}
          </div>
        </div>

        {stats ? (
          <div className="grid grid-cols-[1fr_auto] items-end gap-3">
            <div className="text-primary/80">
              <Sparkline points={stats.points} />
            </div>
            <dl className="grid grid-cols-2 gap-x-3 text-right text-xs">
              <dt className="text-muted-foreground">avg</dt>
              <dd className="font-medium tabular-nums">{stats.avg.toFixed(1)}</dd>
              <dt className="text-muted-foreground">change</dt>
              <dd
                className={cn(
                  "font-medium tabular-nums",
                  stats.change < -0.05 && "text-emerald-600 dark:text-emerald-400",
                  stats.change > 0.05 && "text-rose-600 dark:text-rose-400",
                )}
              >
                {stats.change > 0 ? "+" : stats.change < 0 ? "−" : "±"}
                {Math.abs(stats.change).toFixed(1)}
              </dd>
              <dt className="text-muted-foreground">range</dt>
              <dd className="font-medium tabular-nums">
                {stats.min.toFixed(1)}–{stats.max.toFixed(1)}
              </dd>
            </dl>
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">No readings this month.</p>
        )}
      </CardContent>
    </Card>
  )
}
