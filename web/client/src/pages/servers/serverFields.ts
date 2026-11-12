// Field readers for a SERVER cloud resource, shared by the server pages and the pages that list
// servers (Kubernetes nodes, server detail). Kept out of the page modules: a module that exports
// both components and plain functions cannot be React Fast Refreshed in place.
import type { CloudResource } from "@/lib/types"

export function serverName(r: CloudResource): string {
  return (r.data?.server?.name as string) ?? r.name ?? r.id
}
export function serverStatus(r: CloudResource): string | undefined {
  return (r.data?.server?.status as string) ?? r.status
}
export function serverIPs(r: CloudResource): string[] {
  const addrs = r.data?.server?.addresses as Record<string, Array<{ addr?: string }>> | undefined
  if (!addrs) return []
  return Object.values(addrs)
    .flat()
    .map((a) => a?.addr)
    .filter((x): x is string => !!x)
}
export function serverFlavor(r: CloudResource): string {
  const f = r.data?.server?.flavor as Record<string, unknown> | undefined
  if (!f) return "—"
  const name = (f.original_name as string) ?? (f.name as string)
  const vcpus = f.vcpus as number | undefined
  const ram = f.ram as number | undefined
  if (name && vcpus && ram) return `${name} · ${vcpus} vCPU · ${Math.round(ram / 1024)} GB`
  return name ?? "—"
}
