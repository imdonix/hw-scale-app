import { useEffect, useState } from "react"
import { Loader2, LogOut } from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { Sheet, SheetContent, SheetDescription, SheetFooter, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Switch } from "@/components/ui/switch"
import { api, type Settings, type Status } from "@/lib/api"

function Field({ id, label, hint, children }: { id: string; label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

export function SettingsSheet({
  open,
  onOpenChange,
  status,
  showHidden,
  onShowHidden,
  onSaved,
  onSignOut,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  status: Status | null
  showHidden: boolean
  onShowHidden: (v: boolean) => void
  onSaved: () => void
  onSignOut: () => void
}) {
  const [s, setS] = useState<Settings | null>(null)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!open) return
    api.settings().then(setS, (e) => toast.error((e as Error).message))
  }, [open])

  async function save(e: React.FormEvent) {
    e.preventDefault()
    if (!s) return
    setSaving(true)
    try {
      setS(await api.saveSettings(s))
      toast.success("Settings saved")
      onSaved()
      onOpenChange(false)
    } catch (err) {
      toast.error((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const num = (v: string) => (v === "" ? 0 : Number(v))

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full overflow-y-auto pt-[env(safe-area-inset-top)] sm:max-w-md">
        <SheetHeader>
          <SheetTitle>Settings</SheetTitle>
          <SheetDescription>Your profile is used for BMI and body composition.</SheetDescription>
        </SheetHeader>

        {!s ? (
          <div className="flex justify-center p-8">
            <Loader2 className="animate-spin text-muted-foreground" />
          </div>
        ) : (
          <form id="settings" onSubmit={save} className="flex flex-col gap-5 px-4">
            <Field id="height" label="Height (cm)">
              <Input
                id="height"
                type="number"
                inputMode="decimal"
                min={100}
                max={250}
                step="0.5"
                value={s.height_cm || ""}
                onChange={(e) => setS({ ...s, height_cm: num(e.target.value) })}
              />
            </Field>
            <Field id="birthdate" label="Birthdate">
              <Input
                id="birthdate"
                type="date"
                value={s.birthdate}
                onChange={(e) => setS({ ...s, birthdate: e.target.value })}
              />
            </Field>
            <Field id="sex" label="Sex" hint="Selects the body composition model.">
              <Select value={s.sex || undefined} onValueChange={(v) => setS({ ...s, sex: v as Settings["sex"] })}>
                <SelectTrigger id="sex" className="w-full">
                  <SelectValue placeholder="Select…" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="male">Male</SelectItem>
                  <SelectItem value="female">Female</SelectItem>
                </SelectContent>
              </Select>
            </Field>

            <Separator />

            <div className="flex items-start justify-between gap-4">
              <div className="flex flex-col gap-1">
                <Label htmlFor="filter">Hide readings that are not you</Label>
                <p className="text-xs text-muted-foreground">
                  Readings far from your recent trend (e.g. someone else on the scale) are hidden from the calendar.
                </p>
              </div>
              <Switch
                id="filter"
                checked={s.filter.enabled}
                onCheckedChange={(v) => setS({ ...s, filter: { ...s.filter, enabled: v } })}
              />
            </div>
            {s.filter.enabled && (
              <div className="grid grid-cols-2 gap-3">
                <Field id="tolkg" label="Tolerance (kg)">
                  <Input
                    id="tolkg"
                    type="number"
                    inputMode="decimal"
                    min={0.5}
                    max={30}
                    step="0.5"
                    value={s.filter.tolerance_kg}
                    onChange={(e) => setS({ ...s, filter: { ...s.filter, tolerance_kg: num(e.target.value) } })}
                  />
                </Field>
                <Field id="tolpct" label="or (%)">
                  <Input
                    id="tolpct"
                    type="number"
                    inputMode="decimal"
                    min={0}
                    max={30}
                    step="0.5"
                    value={s.filter.tolerance_pct}
                    onChange={(e) => setS({ ...s, filter: { ...s.filter, tolerance_pct: num(e.target.value) } })}
                  />
                </Field>
                <p className="col-span-2 -mt-1 text-xs text-muted-foreground">
                  The larger of the two applies, plus 0.2 kg per day since your last weigh-in.
                </p>
              </div>
            )}

            <div className="flex items-start justify-between gap-4">
              <div className="flex flex-col gap-1">
                <Label htmlFor="showhidden">Show hidden readings</Label>
                <p className="text-xs text-muted-foreground">Display them crossed out, so you can restore one.</p>
              </div>
              <Switch id="showhidden" checked={showHidden} onCheckedChange={onShowHidden} />
            </div>

            <Separator />

            {status && (
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs text-muted-foreground">
                <dt>Scale</dt>
                <dd className="text-right tabular-nums">{status.scale.name || status.scale.address}</dd>
                {status.device.firmware && (
                  <>
                    <dt>Firmware</dt>
                    <dd className="text-right tabular-nums">{status.device.firmware}</dd>
                  </>
                )}
                <dt>Readings</dt>
                <dd className="text-right tabular-nums">
                  {status.counts.valid} counted, {status.counts.total - status.counts.valid} hidden
                </dd>
              </dl>
            )}
          </form>
        )}

        <SheetFooter className="pb-[max(1rem,env(safe-area-inset-bottom))]">
          <Button type="submit" form="settings" disabled={!s || saving}>
            {saving && <Loader2 className="animate-spin" />}
            Save
          </Button>
          <Button type="button" variant="ghost" onClick={onSignOut}>
            <LogOut /> Sign out
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}
