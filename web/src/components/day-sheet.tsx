import { useState } from "react"
import { Ban, RotateCcw, Trash2, UserCheck } from "lucide-react"
import { toast } from "sonner"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Separator } from "@/components/ui/separator"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import type { DayInfo } from "@/lib/readings"
import { api, type Measurement } from "@/lib/api"
import { fmtDay, fmtTime } from "@/lib/dates"
import { cn } from "@/lib/utils"

function Metric({ label, value, unit }: { label: string; value?: number | null; unit?: string }) {
  if (value === undefined || value === null || Number.isNaN(value)) return null
  return (
    <div className="rounded-lg bg-muted/60 px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="text-sm font-semibold tabular-nums">
        {value}
        {unit && <span className="ml-0.5 font-normal text-muted-foreground">{unit}</span>}
      </div>
    </div>
  )
}

function Reading({ m, onChanged }: { m: Measurement; onChanged: () => void }) {
  const [busy, setBusy] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const c = m.composition

  async function act(fn: () => Promise<unknown>, msg: string) {
    setBusy(true)
    try {
      await fn()
      toast.success(msg)
      onChanged()
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className={cn("flex flex-col gap-3", !m.valid && "opacity-70")}>
      <div className="flex items-baseline justify-between gap-2">
        <div className="flex items-baseline gap-2">
          <span className={cn("text-3xl font-semibold tabular-nums", !m.valid && "line-through")}>
            {m.weight_kg.toFixed(2)}
          </span>
          <span className="text-muted-foreground">kg</span>
        </div>
        <span className="text-sm text-muted-foreground tabular-nums">{fmtTime(new Date(m.time))}</span>
      </div>

      <div className="flex flex-wrap gap-1.5">
        {m.valid ? <Badge variant="secondary">Counted</Badge> : <Badge variant="destructive">Hidden – not you</Badge>}
        {m.manual_valid !== null && <Badge variant="outline">Set by you</Badge>}
        <Badge variant="outline">{m.source}</Badge>
      </div>
      {!m.valid && m.filter_reason && m.manual_valid === null && (
        <p className="text-sm text-muted-foreground">Filtered automatically: {m.filter_reason}</p>
      )}

      <div className="grid grid-cols-3 gap-2">
        <Metric label="BMI" value={m.bmi} />
        <Metric label="Body fat" value={c?.body_fat_percent ?? (m.scale_fat_percent || null)} unit="%" />
        <Metric label="Water" value={c?.water_percent} unit="%" />
        <Metric label="Muscle" value={c ? Number(c.skeletal_muscle_kg.toFixed(1)) : null} unit="kg" />
        <Metric label="Bone" value={c?.bone_mineral_kg} unit="kg" />
        <Metric label="Lean mass" value={c ? Number(c.lean_body_mass_kg.toFixed(1)) : null} unit="kg" />
        <Metric label="Protein" value={c?.protein_percent} unit="%" />
        <Metric label="BMR" value={c?.bmr_kcal} unit="kcal" />
        <Metric label="Visceral fat" value={c?.visceral_fat_level} />
        <Metric label="Impedance" value={m.impedance_ohm} unit="Ω" />
        <Metric label="Heart rate" value={m.heart_rate_bpm || null} unit="bpm" />
      </div>
      {!c && (
        <p className="text-xs text-muted-foreground">
          No body composition: the scale measured no impedance (stand barefoot until it finishes) or your profile is
          incomplete.
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        {m.valid ? (
          <Button variant="outline" size="sm" disabled={busy} onClick={() => act(() => api.setValid(m.id, false), "Hidden from the calendar")}>
            <Ban /> Not me
          </Button>
        ) : (
          <Button variant="outline" size="sm" disabled={busy} onClick={() => act(() => api.setValid(m.id, true), "Counted as yours")}>
            <UserCheck /> That was me
          </Button>
        )}
        {m.manual_valid !== null && (
          <Button variant="ghost" size="sm" disabled={busy} onClick={() => act(() => api.setValid(m.id, null), "Back to automatic")}>
            <RotateCcw /> Automatic
          </Button>
        )}
        <Button variant="ghost" size="sm" className="ml-auto text-destructive" disabled={busy} onClick={() => setConfirmDelete(true)}>
          <Trash2 /> Delete
        </Button>
      </div>

      <Dialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete this reading?</DialogTitle>
            <DialogDescription>
              {m.weight_kg.toFixed(2)} kg at {fmtTime(new Date(m.time))} will be removed permanently. To only hide it, use
              “Not me”.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button
              variant="destructive"
              onClick={() => {
                setConfirmDelete(false)
                act(() => api.remove(m.id), "Reading deleted")
              }}
            >
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

export function DaySheet({
  day,
  onClose,
  onChanged,
}: {
  day: DayInfo | null
  onClose: () => void
  onChanged: () => void
}) {
  const readings = day ? [...day.readings].reverse() : []
  return (
    <Sheet open={!!day} onOpenChange={(o) => !o && onClose()}>
      <SheetContent side="bottom" className="max-h-[85svh] overflow-y-auto rounded-t-2xl pb-[max(1rem,env(safe-area-inset-bottom))]">
        <SheetHeader>
          <SheetTitle>{day ? fmtDay(day.date) : ""}</SheetTitle>
          <SheetDescription>
            {readings.length} reading{readings.length === 1 ? "" : "s"}
          </SheetDescription>
        </SheetHeader>
        <div className="flex flex-col gap-5 px-4">
          {readings.map((m, i) => (
            <div key={m.id} className="flex flex-col gap-5">
              {i > 0 && <Separator />}
              <Reading m={m} onChanged={onChanged} />
            </div>
          ))}
        </div>
      </SheetContent>
    </Sheet>
  )
}
