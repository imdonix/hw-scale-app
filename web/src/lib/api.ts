export interface Composition {
  impedance_ohm: number
  body_fat_percent: number
  water_percent: number
  skeletal_muscle_kg: number
  skeletal_muscle_percent: number
  bone_mineral_kg: number
  lean_body_mass_kg: number
  protein_percent: number
  bmr_kcal: number
  visceral_fat_level: number
}

export interface Measurement {
  id: number
  time: string
  scale_time?: string
  received_at: string
  source: "live" | "latest" | "history"
  weight_kg: number
  scale_fat_percent?: number
  heart_rate_bpm?: number
  auto_valid: boolean
  filter_reason?: string
  manual_valid: boolean | null
  valid: boolean
  bmi?: number
  impedance_ohm?: number
  composition?: Composition
}

export interface Settings {
  height_cm: number
  birthdate: string
  sex: "male" | "female" | ""
  filter: { enabled: boolean; tolerance_kg: number; tolerance_pct: number }
}

export interface Status {
  daemon: { running: boolean; phase: string; started_at: string }
  scale: { address: string; name: string; awake: boolean; last_seen: string | null; rssi: number }
  paired: boolean
  device: { firmware?: string; product_info?: string; unit?: string }
  last_session: { start: string; end: string; result: string; error?: string; records: number } | null
  counts: { total: number; valid: number }
  latest: Measurement | null
}

const KEY_STORAGE = "sscale.apiKey"

export function getApiKey(): string | null {
  try {
    return localStorage.getItem(KEY_STORAGE)
  } catch {
    return null
  }
}

export function setApiKey(key: string | null) {
  try {
    if (key) localStorage.setItem(KEY_STORAGE, key)
    else localStorage.removeItem(KEY_STORAGE)
  } catch {
    /* private mode: key lives for this session only */
  }
}

export class ApiError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown, key = getApiKey()): Promise<T> {
  let res: Response
  try {
    res = await fetch(path, {
      method,
      headers: {
        Authorization: `Bearer ${key ?? ""}`,
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
  } catch {
    throw new ApiError(0, "Cannot reach the sscale server")
  }
  if (res.status === 204) return undefined as T
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new ApiError(res.status, data.error ?? res.statusText)
  return data as T
}

export const api = {
  status: (key?: string) => request<Status>("GET", "/api/status", undefined, key ?? getApiKey()),
  measurements: (from: Date, to: Date, includeInvalid: boolean) =>
    request<Measurement[]>(
      "GET",
      `/api/measurements?from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(
        to.toISOString(),
      )}&include_invalid=${includeInvalid}`,
    ),
  setValid: (id: number, valid: boolean | null) =>
    request<Measurement>("PATCH", `/api/measurements/${id}`, { valid }),
  remove: (id: number) => request<void>("DELETE", `/api/measurements/${id}`),
  settings: () => request<Settings>("GET", "/api/settings"),
  saveSettings: (s: Settings) => request<Settings>("PUT", "/api/settings", s),
}

// Small offline cache so the installed app shows the last data without a
// connection.
export function cacheGet<T>(key: string): T | null {
  try {
    const v = localStorage.getItem(`sscale.cache.${key}`)
    return v ? (JSON.parse(v) as T) : null
  } catch {
    return null
  }
}

export function cacheSet(key: string, value: unknown) {
  try {
    localStorage.setItem(`sscale.cache.${key}`, JSON.stringify(value))
  } catch {
    /* quota or private mode */
  }
}
