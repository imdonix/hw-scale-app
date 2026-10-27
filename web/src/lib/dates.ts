// Local-time calendar helpers (weeks start on Monday).

export function dayKey(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, "0")
  const day = String(d.getDate()).padStart(2, "0")
  return `${d.getFullYear()}-${m}-${day}`
}

export function startOfMonth(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), 1)
}

export function addMonths(d: Date, n: number): Date {
  return new Date(d.getFullYear(), d.getMonth() + n, 1)
}

export function addDays(d: Date, n: number): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n)
}

/** The days shown for a month: whole weeks, Monday first. */
export function monthGrid(month: Date): Date[] {
  const first = startOfMonth(month)
  const start = addDays(first, -((first.getDay() + 6) % 7))
  const last = new Date(first.getFullYear(), first.getMonth() + 1, 0)
  const end = addDays(last, 6 - ((last.getDay() + 6) % 7))
  const days: Date[] = []
  for (let d = start; d <= end; d = addDays(d, 1)) days.push(d)
  return days
}

export function sameMonth(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth()
}

export function weekdayNames(): string[] {
  const fmt = new Intl.DateTimeFormat(undefined, { weekday: "narrow" })
  // 2024-01-01 was a Monday
  return Array.from({ length: 7 }, (_, i) => fmt.format(new Date(2024, 0, 1 + i)))
}

export const fmtMonth = (d: Date) => new Intl.DateTimeFormat(undefined, { month: "long", year: "numeric" }).format(d)
export const fmtDay = (d: Date) =>
  new Intl.DateTimeFormat(undefined, { weekday: "long", day: "numeric", month: "long" }).format(d)
export const fmtTime = (d: Date) => new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(d)
export const fmtShort = (d: Date) => new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short" }).format(d)

export function relative(d: Date, now = new Date()): string {
  const s = Math.round((now.getTime() - d.getTime()) / 1000)
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" })
  if (s < 60) return rtf.format(-s, "second")
  if (s < 3600) return rtf.format(-Math.round(s / 60), "minute")
  if (s < 86400) return rtf.format(-Math.round(s / 3600), "hour")
  return rtf.format(-Math.round(s / 86400), "day")
}
