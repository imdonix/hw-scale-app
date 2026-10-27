import type { Measurement } from "@/lib/api"
import { dayKey } from "@/lib/dates"

export interface DayInfo {
  date: Date
  readings: Measurement[] // all readings of the day (incl. hidden ones if shown)
  weight: number | null // last valid weight of the day
  delta: number | null // vs. the previous day with a valid weight
}

export function groupByDay(readings: Measurement[]): Map<string, Measurement[]> {
  const map = new Map<string, Measurement[]>()
  for (const r of readings) {
    const k = dayKey(new Date(r.time))
    const list = map.get(k)
    if (list) list.push(r)
    else map.set(k, [r])
  }
  return map
}
