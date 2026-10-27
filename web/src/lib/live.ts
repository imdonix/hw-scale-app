import { useEffect, useState } from "react"

import { getApiKey, type Measurement } from "@/lib/api"

export type LivePhase = "idle" | "connecting" | "waiting" | "weighing" | "analyzing" | "saved" | "done" | "failed"

export interface LiveEvent {
  seq: number
  phase: LivePhase
  message?: string
  weight_kg?: number
  measurement?: Measurement
  error?: string
  session_started_at?: string
  updated_at: string
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

/**
 * Follows GET /api/live (Server-Sent Events). It uses fetch rather than
 * EventSource so the API key travels in a header, not in the URL.
 */
export function useLive(): LiveEvent | null {
  const [event, setEvent] = useState<LiveEvent | null>(null)

  useEffect(() => {
    let stopped = false
    let ctrl: AbortController | null = null
    let delay = 1000

    async function follow() {
      while (!stopped) {
        ctrl = new AbortController()
        try {
          const res = await fetch("/api/live", {
            headers: { Authorization: `Bearer ${getApiKey() ?? ""}`, Accept: "text/event-stream" },
            cache: "no-store",
            signal: ctrl.signal,
          })
          if (!res.ok || !res.body) throw new Error(`live stream: ${res.status}`)
          delay = 1000
          const reader = res.body.getReader()
          const decoder = new TextDecoder()
          let buf = ""
          for (;;) {
            const { value, done } = await reader.read()
            if (done) break
            buf += decoder.decode(value, { stream: true })
            let i: number
            while ((i = buf.indexOf("\n\n")) >= 0) {
              const block = buf.slice(0, i)
              buf = buf.slice(i + 2)
              const data = block
                .split("\n")
                .filter((l) => l.startsWith("data: "))
                .map((l) => l.slice(6))
                .join("\n")
              if (data) setEvent(JSON.parse(data) as LiveEvent)
            }
          }
        } catch {
          /* network error, abort or server restart: reconnect below */
        }
        if (stopped) break
        await sleep(delay)
        delay = Math.min(delay * 2, 15_000)
      }
    }
    follow()

    // iOS suspends background tabs; reconnect right away when shown again.
    const onVisible = () => {
      if (document.visibilityState === "visible") {
        delay = 0
        ctrl?.abort()
      }
    }
    document.addEventListener("visibilitychange", onVisible)
    return () => {
      stopped = true
      ctrl?.abort()
      document.removeEventListener("visibilitychange", onVisible)
    }
  }, [])

  return event
}
