// Server console. The browser NEVER talks to the hypervisor console service: each action mints a
// short-lived token and returns ProxCloud's own wss:// endpoint, which reverse-proxies the socket
// server-side (see cloudConsoleProxy). That keeps the PVE console (vncproxy/termproxy) off the public
// internet — only ProxCloud needs to be reachable.
//
// Two consoles, because they solve different problems:
//   Graphical (VNC) — a framebuffer. Shows boot/BIOS output, but the screen is pixels: there is no
//     text to select, and the VNC clipboard needs a guest agent that a bare tty does not have. So
//     pasting is done by SYNTHESISING KEYSTROKES ("Send text").
//   Serial — a plain text stream (the guest's ttyS0) rendered in a real terminal, so selection,
//     copy and paste are native. This is the one to use for command work.
import { useCallback, useEffect, useRef, useState } from "react"
import { Link, useParams } from "react-router-dom"
import RFB from "@novnc/novnc"
import { Terminal } from "@xterm/xterm"
import { FitAddon } from "@xterm/addon-fit"
import "@xterm/xterm/css/xterm.css"
import { ArrowLeft, Keyboard, Maximize, RefreshCw, SendHorizontal } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { apiFetch } from "@/lib/api"
import { useCloudResource, useCloudScope, useProjectId } from "@/lib/hooks"
import { cn } from "@/lib/utils"

type ConsoleState = "connecting" | "connected" | "disconnected" | "error"
type Scope = { serviceId: string; region: string }

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// POST the console action and return the ProxCloud-proxied socket URL (+ the VNC
// password when the backend needs one — Proxmox presents its ticket as the RFB credential).
async function fetchConsoleURL(pid: string, resourceId: string, cloud: Scope, action: string) {
  const res = await apiFetch<{ result?: { url?: string; password?: string } }>(
    `/project/${pid}/cloud/${resourceId}/action`,
    { method: "POST", body: { action }, cloud },
  )
  const url = res?.result?.url
  if (!url) throw new Error("No console URL returned.")
  return { url, password: res?.result?.password }
}

export default function ServerConsolePage() {
  const pid = useProjectId()
  const { resourceId = "" } = useParams()
  const scope = useCloudScope(pid)
  const { data: server } = useCloudResource(pid, resourceId)
  const [tab, setTab] = useState("vnc")

  // useCloudScope builds a fresh object every render, so the console effects depend on these
  // primitives instead — depending on the object reconnects the socket on EVERY render.
  const serviceId = scope?.serviceId
  const region = scope?.region
  const ready = Boolean(pid && resourceId && serviceId && region)

  const name = (server?.data?.server?.name as string) ?? server?.name ?? resourceId

  return (
    <div className="flex min-h-0 flex-col gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="outline" size="sm" asChild>
          <Link to={`/p/${pid}/servers/${resourceId}`}>
            <ArrowLeft className="size-4" /> Back
          </Link>
        </Button>
        <div className="min-w-0">
          <div className="text-eyebrow">Console</div>
          <div className="truncate font-medium">{name}</div>
        </div>
      </div>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="vnc">Graphical (VNC)</TabsTrigger>
          <TabsTrigger value="serial">Serial</TabsTrigger>
        </TabsList>

        {/* Mount-gated so only the visible console holds a socket open. */}
        <TabsContent value="vnc" className="mt-3">
          {ready && tab === "vnc" ? (
            <VncConsole pid={pid} resourceId={resourceId} cloud={{ serviceId: serviceId!, region: region! }} />
          ) : null}
        </TabsContent>
        <TabsContent value="serial" className="mt-3">
          {ready && tab === "serial" ? (
            <SerialConsole pid={pid} resourceId={resourceId} cloud={{ serviceId: serviceId!, region: region! }} />
          ) : null}
        </TabsContent>
      </Tabs>
    </div>
  )
}

// ── Graphical (VNC) ──────────────────────────────────────────────────────────────────────────
function VncConsole({ pid, resourceId, cloud }: { pid: string; resourceId: string; cloud: Scope }) {
  const screenRef = useRef<HTMLDivElement>(null)
  const rfbRef = useRef<RFB | null>(null)
  const [state, setState] = useState<ConsoleState>("connecting")
  const [message, setMessage] = useState("")
  const [attempt, setAttempt] = useState(0)
  const [text, setText] = useState("")
  const [typing, setTyping] = useState(false)
  const [sent, setSent] = useState(0)
  const cancelRef = useRef(false)
  const charCount = text.replace(/\r\n?/g, "\n").length

  const { serviceId, region } = cloud

  useEffect(() => {
    let cancelled = false
    let rfb: RFB | null = null
    let observer: ResizeObserver | null = null
    setState("connecting")
    setMessage("")

    void (async () => {
      try {
        const { url, password } = await fetchConsoleURL(pid, resourceId, { serviceId, region }, "REMOTECONTROL")
        if (cancelled || !screenRef.current) return

        rfb = new RFB(screenRef.current, url, password ? { credentials: { password } } : undefined)
        rfb.scaleViewport = true
        rfb.background = "#000"
        // A text console ships an empty cursor sprite, and noVNC then hides the local pointer
        // entirely — the pointer only reappeared after a click. Draw a dot instead.
        rfb.showDotCursor = true
        rfb.addEventListener("connect", () => {
          if (cancelled) return
          setState("connected")
          // Recompute now that the canvas exists — at construction time the container may still
          // have been mid-layout (zero height), which scales the framebuffer to nothing.
          if (rfbRef.current) rfbRef.current.scaleViewport = true
        })
        rfb.addEventListener("disconnect", (e: Event) => {
          if (cancelled) return
          const clean = (e as CustomEvent<{ clean?: boolean }>).detail?.clean
          setState(clean ? "disconnected" : "error")
          setMessage(clean ? "Console session ended." : "Console connection lost.")
        })
        rfb.addEventListener("securityfailure", (e: Event) => {
          if (cancelled) return
          setState("error")
          setMessage((e as CustomEvent<{ reason?: string }>).detail?.reason ?? "Security handshake failed.")
        })
        // A password-protected VNC server would otherwise sit silently at "Connecting…" waiting
        // for credentials we never supply (the proxy authenticates via the token instead).
        rfb.addEventListener("credentialsrequired", () => {
          if (cancelled) return
          setState("error")
          setMessage("This console requires a VNC password, which ProxCloud does not hold.")
        })
        rfbRef.current = rfb

        // noVNC only re-scales on a WINDOW resize. When the CONTAINER changes size instead
        // (initial layout settling, tab switch, sidebar toggle) the viewport keeps a stale scale
        // and the screen stays blank — which is why entering and leaving fullscreen appeared to
        // "fix" it. Re-assigning scaleViewport runs noVNC's setter, which recomputes immediately.
        let lastW = 0
        let lastH = 0
        observer = new ResizeObserver((entries) => {
          const box = entries[0]?.contentRect
          // Only act on a REAL size change: re-running the setter on every observer callback
          // re-applies the cursor and canvas style, which made the pointer flicker/vanish.
          if (!box || box.width <= 0 || box.height <= 0) return
          if (box.width === lastW && box.height === lastH) return
          lastW = box.width
          lastH = box.height
          if (rfbRef.current) rfbRef.current.scaleViewport = true
        })
        observer.observe(screenRef.current)
      } catch (err) {
        if (cancelled) return
        setState("error")
        setMessage((err as Error).message)
      }
    })()

    return () => {
      cancelled = true
      observer?.disconnect()
      try {
        rfb?.disconnect()
      } catch {
        /* already closed */
      }
      rfbRef.current = null
    }
  }, [pid, resourceId, serviceId, region, attempt])

  // The framebuffer has no clipboard to paste into (a bare tty has no agent), so type the text in
  // as key events. Multi-line is supported: a newline just presses Enter. A small gap between keys
  // keeps a slow tty from dropping characters, which makes a long script slow — hence the progress
  // readout and Cancel.
  const sendText = useCallback(async () => {
    const rfb = rfbRef.current
    if (!rfb || !text) return
    // Normalise CRLF/CR so a script pasted from Windows doesn't press Enter twice per line.
    const chars = Array.from(text.replace(/\r\n?/g, "\n"))
    cancelRef.current = false
    setTyping(true)
    setSent(0)
    try {
      for (let i = 0; i < chars.length; i++) {
        // Bail out on Cancel, or if the socket went away mid-script.
        if (cancelRef.current || !rfbRef.current) break
        const ch = chars[i]
        if (ch === "\n") rfb.sendKey(0xff0d, "Enter")
        else if (ch === "\t") rfb.sendKey(0xff09, "Tab")
        else rfb.sendKey(ch.codePointAt(0)!, null)
        // Throttle the progress state — one re-render per character would churn for a long script.
        if (i % 10 === 0 || i === chars.length - 1) setSent(i + 1)
        await sleep(12)
      }
      if (!cancelRef.current) setText("")
    } finally {
      setTyping(false)
    }
  }, [text])

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <StatusDot state={state} />
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => rfbRef.current?.sendCtrlAltDel()} disabled={state !== "connected"}>
            <Keyboard className="size-4" /> Ctrl+Alt+Del
          </Button>
          <Button variant="outline" size="sm" onClick={() => void screenRef.current?.requestFullscreen?.()} disabled={state !== "connected"}>
            <Maximize className="size-4" /> Fullscreen
          </Button>
          <Button variant="outline" size="sm" onClick={() => setAttempt((n) => n + 1)}>
            <RefreshCw className={cn("size-4", state === "connecting" && "animate-spin")} /> Reconnect
          </Button>
        </div>
      </div>

      {/*
        The parent needs a DEFINITE height and the noVNC target an absolutely-positioned box.
        With only `min-h-*` on the parent, the child's `height:100%` resolves against an `auto`
        height — i.e. zero — so noVNC scaled the framebuffer to nothing and the console rendered
        black. (Fullscreen gave the element a real size, which is why it appeared to fix itself.)
      */}
      <div className="relative h-[60vh] overflow-hidden rounded-xl border bg-black">
        <div ref={screenRef} className="absolute inset-0" />
        {state === "connecting" ? (
          <div className="absolute inset-0 grid place-items-center bg-black/80 p-6">
            <Skeleton className="h-8 w-48" />
          </div>
        ) : null}
        {state === "error" || state === "disconnected" ? (
          <div className="absolute inset-0 grid place-items-center bg-black/80 p-6 text-center">
            <div className="space-y-3">
              <p className="text-sm text-muted-foreground">{message || "Console unavailable."}</p>
              <Button size="sm" onClick={() => setAttempt((n) => n + 1)}>
                <RefreshCw className="size-4" /> Reconnect
              </Button>
            </div>
          </div>
        ) : null}
      </div>

      <div className="grid gap-2">
        <Textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          rows={3}
          className="font-mono text-xs"
          placeholder={"Paste a command or a whole script — every line is typed in, newlines press Enter.\nCtrl+Enter to send."}
          disabled={state !== "connected" || typing}
          aria-label="Text to send to the console"
          onKeyDown={(e) => {
            // Enter inserts a newline (this is a script box); Ctrl/Cmd+Enter sends.
            if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
              e.preventDefault()
              void sendText()
            }
          }}
        />
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" onClick={() => void sendText()} disabled={state !== "connected" || typing || !text}>
            <SendHorizontal className="size-4" /> {typing ? "Sending…" : "Send"}
          </Button>
          {typing ? (
            <>
              <Button variant="outline" size="sm" onClick={() => (cancelRef.current = true)}>
                Cancel
              </Button>
              <span className="font-mono text-xs tabular-nums text-muted-foreground" aria-live="polite">
                {sent}/{charCount} chars
              </span>
            </>
          ) : text ? (
            <span className="font-mono text-xs tabular-nums text-muted-foreground">{charCount} chars</span>
          ) : null}
        </div>
      </div>
      <p className="text-xs text-muted-foreground">
        The graphical console is a framebuffer, so text cannot be selected or copied out of it, and
        typed-in keystrokes have no flow control — a long script can drop characters on a busy
        guest. For anything more than a couple of lines use the Serial tab: it is a real terminal
        with native copy and paste.
      </p>
    </div>
  )
}

// ── Serial ───────────────────────────────────────────────────────────────────────────────────
function SerialConsole({ pid, resourceId, cloud }: { pid: string; resourceId: string; cloud: Scope }) {
  const hostRef = useRef<HTMLDivElement>(null)
  const [state, setState] = useState<ConsoleState>("connecting")
  const [message, setMessage] = useState("")
  const [attempt, setAttempt] = useState(0)

  const { serviceId, region } = cloud

  useEffect(() => {
    let cancelled = false
    let ws: WebSocket | null = null
    let term: Terminal | null = null
    let observer: ResizeObserver | null = null
    setState("connecting")
    setMessage("")

    void (async () => {
      try {
        const { url } = await fetchConsoleURL(pid, resourceId, { serviceId, region }, "SERIAL_CONSOLE")
        if (cancelled || !hostRef.current) return

        term = new Terminal({
          convertEol: true,
          cursorBlink: true,
          fontSize: 13,
          fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace",
          theme: { background: "#000000" },
        })
        const fit = new FitAddon()
        term.loadAddon(fit)
        term.open(hostRef.current)
        fit.fit()

        // Ctrl+C / Ctrl+V belong to the guest shell (SIGINT / literal), so copy & paste use the
        // terminal convention of Ctrl+Shift+C / Ctrl+Shift+V.
        term.attachCustomKeyEventHandler((e) => {
          if (!e.ctrlKey || !e.shiftKey || e.type !== "keydown") return true
          if (e.key === "C" || e.key === "c") {
            const sel = term?.getSelection()
            if (sel) void navigator.clipboard?.writeText(sel)
            return false
          }
          if (e.key === "V" || e.key === "v") {
            void navigator.clipboard?.readText().then((t) => {
              if (t && ws?.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(t))
            })
            return false
          }
          return true
        })

        observer = new ResizeObserver(() => {
          try {
            fit.fit()
          } catch {
            /* container not laid out yet */
          }
        })
        observer.observe(hostRef.current)

        // The serial console proxy speaks websockify's binary sub-protocol.
        ws = new WebSocket(url, ["binary"])
        ws.binaryType = "arraybuffer"
        ws.onopen = () => {
          if (!cancelled) setState("connected")
        }
        ws.onmessage = (e: MessageEvent) => {
          if (!term) return
          if (typeof e.data === "string") term.write(e.data)
          else term.write(new Uint8Array(e.data as ArrayBuffer))
        }
        ws.onerror = () => {
          if (cancelled) return
          setState("error")
          setMessage("Serial console connection failed. Is the serial console enabled for this region?")
        }
        ws.onclose = () => {
          if (cancelled) return
          setState((s) => (s === "error" ? s : "disconnected"))
          setMessage((m) => m || "Serial session ended.")
        }
        term.onData((d) => {
          if (ws?.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(d))
        })
      } catch (err) {
        if (cancelled) return
        setState("error")
        setMessage((err as Error).message)
      }
    })()

    return () => {
      cancelled = true
      observer?.disconnect()
      try {
        ws?.close()
      } catch {
        /* already closed */
      }
      term?.dispose()
    }
  }, [pid, resourceId, serviceId, region, attempt])

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <StatusDot state={state} />
        <div className="ml-auto">
          <Button variant="outline" size="sm" onClick={() => setAttempt((n) => n + 1)}>
            <RefreshCw className={cn("size-4", state === "connecting" && "animate-spin")} /> Reconnect
          </Button>
        </div>
      </div>

      <div className="relative min-h-[55vh] overflow-hidden rounded-xl border bg-black p-2">
        <div ref={hostRef} className="size-full min-h-[52vh]" />
        {state === "error" || state === "disconnected" ? (
          <div className="absolute inset-0 grid place-items-center bg-black/80 p-6 text-center">
            <div className="space-y-3">
              <p className="text-sm text-muted-foreground">{message || "Serial console unavailable."}</p>
              <Button size="sm" onClick={() => setAttempt((n) => n + 1)}>
                <RefreshCw className="size-4" /> Reconnect
              </Button>
            </div>
          </div>
        ) : null}
      </div>
      <p className="text-xs text-muted-foreground">
        Select text to copy with Ctrl+Shift+C, paste with Ctrl+Shift+V — plain Ctrl+C still goes to
        the guest as SIGINT. Press Enter if the login prompt has not been drawn yet.
      </p>
    </div>
  )
}

function StatusDot({ state }: { state: ConsoleState }) {
  // Distinct wording per state — the dot colour alone doesn't reach assistive tech, and a clean
  // disconnect reads very differently from a failure.
  const label = {
    connected: "Connected",
    connecting: "Connecting…",
    disconnected: "Disconnected",
    error: "Connection failed",
  }[state]
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
      <span
        aria-hidden
        className={cn(
          "size-2 rounded-full",
          state === "connected" && "bg-emerald-500",
          state === "connecting" && "bg-amber-500",
          (state === "error" || state === "disconnected") && "bg-destructive",
        )}
      />
      {label}
    </span>
  )
}
