import { useEffect, useState } from "react"
import { Activity, AlertTriangle, Bluetooth, CheckCircle2, Footprints, Scale, UserCheck, X } from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { api } from "@/lib/api"
import type { LiveEvent, LivePhase } from "@/lib/live"
import { cn } from "@/lib/utils"

const AUTO_HIDE_MS = 7000
const TRANSITION_MS = 300

const steps: readonly (readonly LivePhase[])[] = [["connecting"], ["waiting", "weighing"], ["analyzing"], ["saved", "done"]]

function stepIndex(e: LiveEvent): number {
  if (e.phase === "done" && !e.measurement) return 1
  return steps.findIndex((s) => s.includes(e.phase))
}

function texts(e: LiveEvent): [string, string] {
  const m = e.measurement
  switch (e.phase) {
    case "connecting":
      return ["Scale woke up", "Connecting…"]
    case "waiting":
      return ["Step on the scale", "Waiting for your weigh-in"]
    case "weighing":
      return ["Weighing", "Finishing the measurement"]
    case "analyzing":
      return ["Analyzing body composition", "Stay on the scale"]
    case "saved":
    case "done":
      if (!m) return ["Finished", "No new reading"]
      return m.valid ? ["Saved", "Added to your calendar"] : ["Not counted", "Doesn't match your trend"]
    case "failed":
      return ["Couldn't read the scale", e.error || e.message || "Unknown error"]
    default:
      return ["", ""]
  }
}

function PhaseIcon({ phase, hidden }: { phase: LivePhase; hidden?: boolean }) {
  const base = "relative flex size-10 shrink-0 items-center justify-center rounded-full"
  switch (phase) {
    case "connecting":
      return (
        <span className={cn(base, "bg-sky-500/15 text-sky-500")}>
          <span className="absolute inset-0 animate-ping rounded-full bg-sky-500/20" />
          <Bluetooth className="size-5" />
        </span>
      )
    case "waiting":
      return (
        <span className={cn(base, "bg-amber-500/15 text-amber-500")}>
          <Footprints className="size-5 animate-bounce" />
        </span>
      )
    case "weighing":
      return (
        <span className={cn(base, "bg-primary/10 text-primary")}>
          <Scale className="size-5" />
        </span>
      )
    case "analyzing":
      return (
        <span className={cn(base, "bg-violet-500/15 text-violet-500")}>
          <span className="absolute inset-0 animate-spin rounded-full border-2 border-transparent border-t-violet-500" />
          <Activity className="size-5" />
        </span>
      )
    case "failed":
      return (
        <span className={cn(base, "bg-destructive/15 text-destructive")}>
          <AlertTriangle className="size-5" />
        </span>
      )
    default:
      return (
        <span className={cn(base, hidden ? "bg-muted text-muted-foreground" : "bg-emerald-500/15 text-emerald-500")}>
          <CheckCircle2 className="size-5" />
        </span>
      )
  }
}

/**
 * Live activity: slides open under the header while the daemon talks to
 * the scale and closes a few seconds after the session ends. It is part of
 * the page flow (pushing content down, never covering it) and sticks to the
 * top while scrolling.
 */
export function LiveActivity({ event, onChanged }: { event: LiveEvent | null; onChanged: () => void }) {
  const session = event?.session_started_at ?? null
  const [dismissed, setDismissed] = useState<string | null>(null)
  const active = !!event && event.phase !== "idle" && session !== dismissed
  const finished = event?.phase === "done" || event?.phase === "failed"

  // Mounted content (kept during the closing transition) and open state.
  const [shown, setShown] = useState<LiveEvent | null>(null)
  const [open, setOpen] = useState(false)
  useEffect(() => {
    // Drives the open/close transition (mount, animate, unmount).
    if (active) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setShown(event)
      const raf = requestAnimationFrame(() => setOpen(true))
      return () => cancelAnimationFrame(raf)
    }
    setOpen(false)
    const t = setTimeout(() => setShown(null), TRANSITION_MS)
    return () => clearTimeout(t)
  }, [active, event])

  useEffect(() => {
    if (!finished || !session) return
    const t = setTimeout(() => setDismissed(session), AUTO_HIDE_MS)
    return () => clearTimeout(t)
  }, [finished, session])

  const [busy, setBusy] = useState(false)
  if (!shown) return null

  const e = shown
  const m = e.measurement
  const idx = stepIndex(e)
  const weight = m?.weight_kg ?? e.weight_kg
  const [title, subtitle] = texts(e)
  const done = e.phase === "saved" || e.phase === "done"

  async function markMine() {
    if (!m) return
    setBusy(true)
    try {
      await api.setValid(m.id, true)
      toast.success("Counted as yours")
      onChanged()
      setDismissed(session)
    } catch (err) {
      toast.error((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const metrics = m
    ? [
        m.bmi && `BMI ${m.bmi.toFixed(1)}`,
        m.composition && `fat ${m.composition.body_fat_percent.toFixed(1)}%`,
        m.composition && `muscle ${m.composition.skeletal_muscle_kg.toFixed(1)} kg`,
        m.composition && `water ${m.composition.water_percent.toFixed(1)}%`,
      ].filter(Boolean)
    : []

  return (
    <div
      className={cn(
        "sticky top-[max(0.5rem,env(safe-area-inset-top))] z-30 -mb-4 grid transition-all ease-out",
        open ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] -translate-y-2 opacity-0",
      )}
      style={{ transitionDuration: `${TRANSITION_MS}ms` }}
    >
      <div className="overflow-hidden">
        <div
          role="status"
          aria-live="polite"
          className="mb-4 rounded-2xl border bg-card/80 p-3 shadow-lg ring-1 ring-black/5 backdrop-blur-xl dark:ring-white/5"
        >
          <div className="flex items-center gap-3">
            <PhaseIcon phase={e.phase} hidden={m ? !m.valid : false} />
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-semibold">{title}</div>
              <div className="truncate text-xs text-muted-foreground">{subtitle}</div>
            </div>
            {weight ? (
              <div
                key={weight}
                className={cn(
                  "animate-in fade-in-0 zoom-in-95 text-2xl leading-none font-semibold tabular-nums duration-300",
                  m && !m.valid && "text-muted-foreground line-through",
                )}
              >
                {weight.toFixed(2)}
                <span className="ml-0.5 inline-block text-sm font-normal text-muted-foreground">kg</span>
              </div>
            ) : null}
            <Button
              variant="ghost"
              size="icon-sm"
              className="-mr-1 shrink-0 text-muted-foreground"
              aria-label="Dismiss"
              onClick={() => setDismissed(session)}
            >
              <X />
            </Button>
          </div>

          {(metrics.length > 0 || (m && !m.valid)) && (
            <div className="mt-2 flex items-center gap-2 pl-[3.25rem]">
              <p className="min-w-0 flex-1 truncate text-xs text-muted-foreground tabular-nums">{metrics.join(" · ")}</p>
              {m && !m.valid && (
                <Button variant="outline" size="xs" disabled={busy} onClick={markMine}>
                  <UserCheck /> That was me
                </Button>
              )}
            </div>
          )}

          {e.phase !== "failed" && (
            <div className="mt-2.5 grid grid-cols-4 gap-1 pl-[3.25rem]" aria-hidden>
              {steps.map((_, i) => (
                <div key={i} className="h-1 overflow-hidden rounded-full bg-muted">
                  <div
                    className={cn(
                      "h-full rounded-full transition-all duration-500",
                      i < idx || (i === idx && done)
                        ? "w-full bg-emerald-500"
                        : i === idx
                          ? "w-1/2 animate-pulse bg-primary"
                          : "w-0",
                    )}
                  />
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
