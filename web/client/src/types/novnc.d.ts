// noVNC ships plain ESM with no bundled types — declare the slice of the RFB API we use.
// See https://github.com/novnc/noVNC/blob/master/docs/API.md
// The package exports a single root entry ("exports": "./core/rfb.js"), so the bare
// specifier is the only importable path.
declare module "@novnc/novnc" {
  export default class RFB extends EventTarget {
    constructor(
      target: Element,
      url: string,
      options?: {
        shared?: boolean
        credentials?: { username?: string; password?: string; target?: string }
        repeaterID?: string
        wsProtocols?: string[]
      },
    )
    /** Scale the remote framebuffer to fit the container instead of clipping. */
    scaleViewport: boolean
    /** Ask the server to resize its session to the container (needs server support). */
    resizeSession: boolean
    viewOnly: boolean
    focusOnClick: boolean
    background: string
    /** Draw a dot when the remote cursor is invisible (a text console sends an empty sprite). */
    showDotCursor: boolean
    disconnect(): void
    focus(): void
    blur(): void
    sendCtrlAltDel(): void
    /** Synthesise a key press. `code` may be null to send the keysym alone. */
    sendKey(keysym: number, code?: string | null, down?: boolean): void
    machineReboot(): void
    clipboardPasteFrom(text: string): void
  }
}
