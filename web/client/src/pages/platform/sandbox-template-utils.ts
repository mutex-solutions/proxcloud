// Convenience checks only: the server enforces operator-approved origins, exact
// repositories (including project substitution), access, architecture and limits.
export const templateNamePattern = /^[a-z0-9][a-z0-9._-]{0,62}$/
export function validImageSource(source: string) {
  if (source.length > 512 || /\s/.test(source)) return false
  // Immutable OCI reference syntax, not a registry allow-list. No scheme,
  // credentials, tag, query or fragment. Registry ports are syntactically valid.
  return /^(?:[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::[0-9]{1,5})?\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*@sha256:[a-f0-9]{64}$/.test(source)
}
export const validRuntimeUser = (user: string) => user === "" || /^[a-zA-Z_][a-zA-Z0-9_.-]{0,31}$/.test(user)
export const validWorkdir = (workdir: string) => new TextEncoder().encode(workdir).length <= 4096 && !["\0", "\r", "\n"].some((character) => workdir.includes(character)) && (!workdir || workdir.startsWith("/"))
export const validSize = (value: string, max: number) => Number.isInteger(Number(value)) && Number(value) >= 1 && Number(value) <= max
export const validCommand = (value: string) => !value.includes("\0") && new TextEncoder().encode(value).length <= 32768
export const validRegistryCredentials = (username: string, password: string) =>
  !!username && !!password && new TextEncoder().encode(username).length <= 1024 && new TextEncoder().encode(password).length <= 32768 &&
  ![":", "\0", "\r", "\n"].some((character) => username.includes(character)) && !["\0", "\r", "\n"].some((character) => password.includes(character))

export function sandboxTemplatePath(pid: string, ...segments: string[]) {
  const segment = (part: string) => {
    if (!part || part === "." || part === "..") throw new Error("Invalid project or template identifier")
    return encodeURIComponent(part)
  }
  return `/project/${segment(pid)}/sandbox-templates${segments.map((part) => `/${segment(part)}`).join("")}`
}

// Untrusted terminal output stays plain React text, never HTML. Strip OSC
// (including hyperlinks), CSI and controls; also discard unterminated OSC tails.
export function plainBuildText(value: string) {
  /* eslint-disable no-control-regex */
  return value.replace(/(?:\x1b\]|\x9d)[\s\S]*?(?:\x07|\x1b\\|\x9c|$)/g, "")
    .replace(/(?:\x1b\[|\x9b)[0-?]*[ -/]*[@-~]/g, "")
    .replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, "")
}
export const activeBuild = (status?: string) => status === "waiting" || status === "building"
