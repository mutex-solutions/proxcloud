// Theme state: light | dark | system. The pre-paint script in index.html
// applies the initial class before first paint (no flash); this module owns
// everything after — persistence, live system-preference tracking, and the
// View Transition circle reveal on manual toggles.
import { useCallback, useSyncExternalStore } from "react"

export type ThemePref = "light" | "dark" | "system"

const STORAGE_KEY = "proxcloud.theme"

function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    return v === "light" || v === "dark" ? v : "system"
  } catch {
    return "system"
  }
}

function systemDark(): boolean {
  return window.matchMedia("(prefers-color-scheme: dark)").matches
}

function resolveDark(pref: ThemePref): boolean {
  return pref === "dark" || (pref === "system" && systemDark())
}

// The class toggle must be synchronous (View Transitions snapshot the DOM
// inside the callback), and always on <html> — token chains re-resolve in
// dark mode only because .dark sits on :root.
function apply(pref: ThemePref) {
  const dark = resolveDark(pref)
  document.documentElement.classList.toggle("dark", dark)
  // Keep browser chrome in sync with the RESOLVED theme (the static media
  // metas only track the OS preference).
  const meta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]:not([media])')
  if (meta) meta.content = dark ? "#0c0b09" : "#FFFDF6"
}

const listeners = new Set<() => void>()
function emit() {
  for (const l of listeners) l()
}
function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

// Live-follow OS theme changes while in system mode.
if (typeof window !== "undefined") {
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
    if (readPref() === "system") {
      apply("system")
      emit()
    }
  })
  // Cross-tab sync: a theme change in another tab updates this one live.
  window.addEventListener("storage", (e) => {
    if (e.key === STORAGE_KEY || e.key === null) {
      apply(readPref())
      emit()
    }
  })
}

type ViewTransitionDocument = Document & {
  startViewTransition?: (cb: () => void) => { ready: Promise<void> }
}

/** Set the theme preference, with a circle-reveal View Transition when
 * supported (skipped under prefers-reduced-motion). Pass the toggle button's
 * center as `origin` for the reveal to grow from it. */
export function setThemePref(pref: ThemePref, origin?: { x: number; y: number }) {
  const commit = () => {
    try {
      if (pref === "system") localStorage.removeItem(STORAGE_KEY)
      else localStorage.setItem(STORAGE_KEY, pref)
    } catch {
      // storage unavailable — theme still applies for this session
    }
    apply(pref)
    emit()
  }

  const doc = document as ViewTransitionDocument
  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches
  if (!doc.startViewTransition || reduceMotion) {
    commit()
    return
  }

  const transition = doc.startViewTransition(commit)
  if (origin) {
    transition.ready
      .then(() => {
        const endRadius = Math.hypot(
          Math.max(origin.x, window.innerWidth - origin.x),
          Math.max(origin.y, window.innerHeight - origin.y),
        )
        document.documentElement.animate(
          {
            clipPath: [
              `circle(0px at ${origin.x}px ${origin.y}px)`,
              `circle(${endRadius}px at ${origin.x}px ${origin.y}px)`,
            ],
          },
          { duration: 500, easing: "ease-in-out", pseudoElement: "::view-transition-new(root)" },
        )
      })
      .catch(() => {
        // transition was skipped/aborted — theme is already applied
      })
  }
}

export function useTheme() {
  const pref = useSyncExternalStore(subscribe, readPref)
  const dark = useSyncExternalStore(subscribe, () =>
    document.documentElement.classList.contains("dark"),
  )
  const setPref = useCallback(
    (next: ThemePref, origin?: { x: number; y: number }) => setThemePref(next, origin),
    [],
  )
  return { pref, dark, setPref }
}
