// The rule for a server's name, the same as the API's pve.ValidVMName: PVE's dns-name format, with a DNS name's limits.
const LABEL = /^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$/

export const SERVER_NAME_RULE =
  "Use letters, digits and hyphens, with dots between parts. Each part starts and ends with a letter or digit."

export function isValidServerName(name: string): boolean {
  if (name.length > 253) return false
  return name.split(".").every((part) => part.length <= 63 && LABEL.test(part))
}
