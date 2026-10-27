import { useState } from "react"
import { KeyRound, Loader2, Scale } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { api, ApiError, setApiKey } from "@/lib/api"

export function Login({ onLogin }: { onLogin: () => void }) {
  const [key, setKey] = useState("")
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    const k = key.trim()
    if (!k) return
    setBusy(true)
    setError(null)
    try {
      await api.status(k)
      setApiKey(k)
      onLogin()
    } catch (err) {
      setError(err instanceof ApiError && err.status === 401 ? "That key is not valid." : String((err as Error).message))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-svh items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <div className="mb-2 flex size-11 items-center justify-center rounded-xl bg-primary text-primary-foreground">
            <Scale className="size-6" />
          </div>
          <CardTitle className="text-xl">Weight tracker</CardTitle>
          <CardDescription>
            Enter the API key of your sscale server. It is printed in the server log at startup, or run{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">./sscale apikey</code>.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="flex flex-col gap-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="key">API key</Label>
              <Input
                id="key"
                value={key}
                onChange={(e) => setKey(e.target.value)}
                placeholder="ssk_…"
                autoComplete="current-password"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                aria-invalid={!!error}
              />
              {error && <p className="text-sm text-destructive">{error}</p>}
            </div>
            <Button type="submit" size="lg" disabled={busy || !key.trim()}>
              {busy ? <Loader2 className="animate-spin" /> : <KeyRound />}
              Connect
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
