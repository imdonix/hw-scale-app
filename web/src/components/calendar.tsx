import { useMemo } from "react"

import { cn } from "@/lib/utils"
import type { Measurement } from "@/lib/api"
import { dayKey, monthGrid, sameMonth, weekdayNames } from "@/lib/dates"
import { groupByDay, type DayInfo } from "@/lib/readings"

export function Calendar({
  month,
  today,
  readings,
  onSelect,
}: {
  month: Date
  today: Date
  readings: Measurement[]
  onSelect: (day: DayInfo) => void
}) {
  const days = useMemo(() => {
    const byDay = groupByDay(readings)
    const grid = monthGrid(month)
    // Seed with the last valid reading before the grid so the first
    // weigh-in shown also gets a delta.
    let prev: number | null = null
    for (const r of readings) {
      if (r.valid && new Date(r.time) < grid[0]) prev = r.weight_kg
    }
    const out: DayInfo[] = []
    for (const date of grid) {
      const list = byDay.get(dayKey(date)) ?? []
      const valid = list.filter((r) => r.valid)
      const weight = valid.length ? valid[valid.length - 1].weight_kg : null
      const delta = weight !== null && prev !== null ? weight - prev : null
      if (weight !== null) prev = weight
      out.push({ date, readings: list, weight, delta })
    }
    return out
  }, [month, readings])

  const todayKey = dayKey(today)
  const names = useMemo(() => weekdayNames(), [])

  return (
    <div className="select-none">
      <div className="mb-1 grid grid-cols-7 text-center text-xs font-medium text-muted-foreground">
        {names.map((n, i) => (
          <div key={i} className="py-1">
            {n}
          </div>
        ))}
      </div>
      <div className="grid grid-cols-7 gap-1">
        {days.map((d) => {
          const inMonth = sameMonth(d.date, month)
          const hidden = d.readings.filter((r) => !r.valid).length
          const hasData = d.readings.length > 0
          const isToday = dayKey(d.date) === todayKey
          return (
            <button
              key={dayKey(d.date)}
              type="button"
              disabled={!hasData}
              onClick={() => onSelect(d)}
              className={cn(
                "relative flex aspect-[4/5] min-w-0 flex-col items-stretch rounded-lg border p-1 text-left transition-colors sm:aspect-square sm:p-1.5",
                inMonth ? "bg-card" : "bg-transparent opacity-40",
                hasData ? "cursor-pointer hover:bg-accent active:bg-accent" : "border-transparent",
                isToday && "ring-2 ring-primary",
              )}
            >
              <span
                className={cn(
                  "text-[11px] leading-none tabular-nums sm:text-xs",
                  isToday ? "font-semibold text-foreground" : "text-muted-foreground",
                )}
              >
                {d.date.getDate()}
              </span>
              <span className="mt-auto flex flex-col items-center">
                {d.weight !== null ? (
                  <span className="text-[13px] leading-tight font-semibold tabular-nums sm:text-base">
                    {d.weight.toFixed(1)}
                  </span>
                ) : hasData ? (
                  <span className="text-[12px] leading-tight text-muted-foreground tabular-nums line-through sm:text-sm">
                    {d.readings[d.readings.length - 1].weight_kg.toFixed(1)}
                  </span>
                ) : null}
                {/* Always reserve the delta line so weights stay aligned. */}
                <span
                  className={cn(
                    "text-[10px] leading-tight tabular-nums sm:text-xs",
                    d.delta !== null && d.delta < 0 ? "text-emerald-600 dark:text-emerald-400" : "text-rose-600 dark:text-rose-400",
                  )}
                >
                  {d.delta !== null && Math.abs(d.delta) >= 0.05 ? (
                    <>
                      {d.delta > 0 ? "+" : "−"}
                      {Math.abs(d.delta).toFixed(1)}
                    </>
                  ) : d.delta !== null ? (
                    <span className="text-muted-foreground">±0</span>
                  ) : (
                    "\u00a0"
                  )}
                </span>
              </span>
              {(d.readings.length > 1 || hidden > 0) && (
                <span className="absolute top-1 right-1 flex gap-0.5">
                  {d.readings.slice(0, 3).map((r) => (
                    <span
                      key={r.id}
                      className={cn("size-1 rounded-full", r.valid ? "bg-primary" : "bg-muted-foreground/50")}
                    />
                  ))}
                </span>
              )}
            </button>
          )
        })}
      </div>
    </div>
  )
}
