// Permission match supports the wildcard grants /admin/me can carry
// (admin:* or admin:<area>:*), mirroring the API's ExpandPatterns.
export function hasPermission(granted: string[] | undefined, want?: string): boolean {
  if (!want) return true
  if (!granted) return true // still loading — don't flash-hide
  if (granted.includes(want) || granted.includes("admin:*")) return true
  const area = want.split(":").slice(0, 2).join(":")
  return granted.includes(`${area}:*`)
}
