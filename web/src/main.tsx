import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { ThemeProvider } from "next-themes"
import { registerSW } from "virtual:pwa-register"

import "./index.css"
import App from "./App.tsx"
import { Toaster } from "@/components/ui/sonner"

// Service worker: offline app shell; new versions activate on next launch.
registerSW({ immediate: true })

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange>
      <App />
      <Toaster position="top-center" />
    </ThemeProvider>
  </StrictMode>,
)
