import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { ChevronLeft, ChevronRight, Settings as SettingsIcon, WifiOff } from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { Calendar } from "@/components/calendar"
import { DaySheet } from "@/components/day-sheet"
import { LiveActivity } from "@/components/live-activity"
import { Login } from "@/components/login"
import { SettingsSheet } from "@/components/settings-sheet"
import { Summary } from "@/components/summary"
import { api, ApiError, cacheGet, cacheSet, getApiKey, setApiKey, type Measurement, type Status } from "@/lib/api"
import { addDays, addMonths, dayKey, fmtMonth, monthGrid, sameMonth, startOfMonth } from "@/lib/dates"
import { useLive } from "@/lib/live"
import { groupByDay, type DayInfo } from "@/lib/readings"

const SHOW_HIDDEN = "sscale.showHidden"
const REFRESH_MS = 30_000

function readShowHidden() {
  try {
    return localStorage.getItem(SHOW_HIDDEN) === "1"
  } catch {
    return false
  }
}

function Tracker({ onSignOut }: { onSignOut: () => void }) {
  const [month, setMonth] = useState(() => startOfMonth(new Date()))
  const [showHidden, setShowHidden] = useState(readShowHidden)
  const cacheKey = `${dayKey(month)}.${showHidden}`
  const [readings, setReadings] = useState<Measurement[]>(() => cacheGet<Measurement[]>(cacheKey) ?? [])
  const [status, setStatus] = useState<Status | null>(() => cacheGet<Status>("status"))
  const [offline, setOffline] = useState(false)
  const [selected, setSelected] = useState<Date | null>(null)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [today, setToday] = useState(() => new Date())

  const load = useCallback(async () => {
    // Fetch a month before the grid too, so the first weigh-in gets a delta.
    const grid = monthGrid(month)
    const from = addDays(grid[0], -31)
    const to = addDays(grid[grid.length - 1], 1)
    try {
      const [ms, st] = await Promise.all([api.measurements(from, to, showHidden), api.status()])
      setReadings(ms)
      setStatus(st)
      setToday(new Date())
      setOffline(false)
      cacheSet(cacheKey, ms)
      cacheSet("status", st)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        toast.error("The API key is no longer valid")
        onSignOut()
        return
      }
      setOffline(true)
      const cached = cacheGet<Measurement[]>(cacheKey)
      if (cached) setReadings(cached)
    }
  }, [month, showHidden, cacheKey, onSignOut])

  useEffect(() => {
    // Sync with the server: initial load, periodic refresh, and on resume.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    load()
    const id = window.setInterval(load, REFRESH_MS)
    const onVisible = () => document.visibilityState === "visible" && load()
    document.addEventListener("visibilitychange", onVisible)
    window.addEventListener("online", load)
    return () => {
      window.clearInterval(id)
      document.removeEventListener("visibilitychange", onVisible)
      window.removeEventListener("online", load)
    }
  }, [load])

  // Live scale session: refresh the calendar as soon as a reading is saved.
  const live = useLive()
  const savedKey = live && (live.phase === "saved" || live.phase === "done") ? `${live.session_started_at}:${live.measurement?.id ?? ""}` : ""
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (savedKey) load()
  }, [savedKey, load])

  const selectedDay: DayInfo | null = useMemo(() => {
    if (!selected) return null
    const list = groupByDay(readings).get(dayKey(selected)) ?? []
    return list.length ? { date: selected, readings: list, weight: null, delta: null } : null
  }, [selected, readings])

  // Swipe left/right to change the month.
  const touch = useRef<{ x: number; y: number } | null>(null)
  const onTouchStart = (e: React.TouchEvent) => (touch.current = { x: e.touches[0].clientX, y: e.touches[0].clientY })
  const onTouchEnd = (e: React.TouchEvent) => {
    if (!touch.current) return
    const dx = e.changedTouches[0].clientX - touch.current.x
    const dy = e.changedTouches[0].clientY - touch.current.y
    touch.current = null
    if (Math.abs(dx) > 60 && Math.abs(dx) > 2 * Math.abs(dy)) setMonth((m) => addMonths(m, dx < 0 ? 1 : -1))
  }

  const isCurrentMonth = sameMonth(month, today)

  return (
    <div className="mx-auto flex min-h-svh max-w-xl flex-col gap-4 px-3 pt-[max(0.75rem,env(safe-area-inset-top))] pb-[max(1rem,env(safe-area-inset-bottom))] sm:px-4">
      <header className="flex items-center justify-between">
        <h1 className="text-lg font-semibold tracking-tight">Weight</h1>
        <div className="flex items-center gap-1">
          {offline && (
            <span className="flex items-center gap-1 text-xs text-muted-foreground" title="Showing cached data">
              <WifiOff className="size-3.5" /> offline
            </span>
          )}
          <Button variant="ghost" size="icon" aria-label="Settings" onClick={() => setSettingsOpen(true)}>
            <SettingsIcon />
          </Button>
        </div>
      </header>

      <LiveActivity event={live} onChanged={load} />
      <Summary month={month} readings={readings} status={status} live={live} />

      <section onTouchStart={onTouchStart} onTouchEnd={onTouchEnd} className="flex flex-col gap-2">
        <div className="flex items-center justify-between">
          <Button variant="ghost" size="icon" aria-label="Previous month" onClick={() => setMonth((m) => addMonths(m, -1))}>
            <ChevronLeft />
          </Button>
          <button
            type="button"
            className="rounded-md px-2 py-1 text-base font-semibold capitalize"
            onClick={() => setMonth(startOfMonth(today))}
            title="Back to this month"
          >
            {fmtMonth(month)}
          </button>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Next month"
            disabled={isCurrentMonth}
            onClick={() => setMonth((m) => addMonths(m, 1))}
          >
            <ChevronRight />
          </Button>
        </div>
        <Calendar month={month} today={today} readings={readings} onSelect={(d) => setSelected(d.date)} />
      </section>

      <DaySheet day={selectedDay} onClose={() => setSelected(null)} onChanged={load} />
      <SettingsSheet
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
        status={status}
        showHidden={showHidden}
        onShowHidden={(v) => {
          setShowHidden(v)
          try {
            localStorage.setItem(SHOW_HIDDEN, v ? "1" : "0")
          } catch {
            /* ignore */
          }
        }}
        onSaved={load}
        onSignOut={onSignOut}
      />
    </div>
  )
}

export default function App() {
  const [authed, setAuthed] = useState(() => !!getApiKey())
  const signOut = useCallback(() => {
    setApiKey(null)
    setAuthed(false)
  }, [])
  return authed ? <Tracker onSignOut={signOut} /> : <Login onLogin={() => setAuthed(true)} />
}
