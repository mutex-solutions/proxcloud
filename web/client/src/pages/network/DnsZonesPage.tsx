import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Globe, ListTree, Plus, RefreshCw, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"
import { useCloudList, useLocations, useProjectId } from "@/lib/hooks"
import type { CloudResource } from "@/lib/types"

const zone = (r: CloudResource) => (r.data?.zone as Record<string, any>) ?? {}
type RRSet = { name: string; type: string; ttl?: number; records: Array<{ content: string }> }
const TYPES = ["A", "AAAA", "CAA", "CNAME", "MX", "NS", "PTR", "SRV", "TXT"]
const emptyRecord = { name: "", type: "A", ttl: "3600", content: "" }

export default function DnsZonesPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { data, isLoading, refetch, isFetching } = useCloudList(pid, "DNS_ZONE")
  const locations = useLocations(pid).data ?? []
  const location = locations.find((l) => l.resourceTypes?.includes("DNS_ZONE"))
  const scope = { serviceId: location?.serviceId ?? "", region: location?.region ?? "" }
  const rows = data ?? []

  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [open, setOpen] = useState<CloudResource | null>(null)
  const [record, setRecord] = useState(emptyRecord)
  const [deleting, setDeleting] = useState<CloudResource | null>(null)
  const [deletingRecord, setDeletingRecord] = useState<RRSet | null>(null)

  const refresh = () => qc.invalidateQueries({ queryKey: ["cloud", pid, "DNS_ZONE"] }).then(() => refetch())
  const action = <T,>(r: CloudResource, act: string, body?: Record<string, unknown>) =>
    apiFetch<{ result: T }>(`/project/${pid}/cloud/${r.id}/action`, {
      method: "POST",
      body: { action: act, ...(body ? { data: body } : {}) },
    })

  const records = useQuery({
    queryKey: ["dns-records", open?.id],
    queryFn: async () => (await action<RRSet[]>(open!, "LIST_RECORDS")).result ?? [],
    enabled: !!open,
  })
  const create = useMutation({
    mutationFn: () =>
      apiFetch<CloudResource>(`/project/${pid}/cloud`, {
        method: "POST",
        cloud: scope,
        body: { type: "DNS_ZONE", serviceId: scope.serviceId, data: { name: name.trim() } },
      }),
    onSuccess: () => {
      toast.success("Zone created")
      setCreateOpen(false)
      setName("")
      void refresh()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const save = useMutation({
    mutationFn: () =>
      action(open!, "SET_RECORD", {
        name: record.name.trim(),
        type: record.type,
        ttl: Number(record.ttl),
        records: record.content.split("\n").map((l) => l.trim()).filter(Boolean),
      }),
    onSuccess: () => {
      toast.success("Record saved")
      setRecord(emptyRecord)
      void records.refetch()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const removeRecord = useMutation({
    mutationFn: (r: RRSet) => action(open!, "DELETE_RECORD", { name: r.name, type: r.type }),
    onSuccess: () => {
      setDeletingRecord(null)
      void records.refetch()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const remove = useMutation({
    mutationFn: () => apiFetch(`/project/${pid}/cloud/${deleting!.id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Zone deleted")
      setDeleting(null)
      void refresh()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const columns = useMemo<ColumnDef<CloudResource>[]>(
    () => [
      { id: "name", header: sortableHeader("Zone"), accessorFn: (r) => zone(r).name ?? "" },
      { id: "ns", header: "Name servers", accessorFn: (r) => ((zone(r).nameservers as string[]) ?? []).join(", ") },
      { id: "created", header: "Created", accessorFn: (r) => timeAgo(r.createdAt) },
      {
        id: "actions",
        header: "",
        cell: ({ row }) => (
          <div className="flex justify-end gap-1">
            <Button variant="ghost" size="sm" onClick={() => setOpen(row.original)}>
              <ListTree className="size-4" /> Records
            </Button>
            <Button variant="ghost" size="sm" aria-label="Delete" onClick={() => setDeleting(row.original)}>
              <Trash2 className="size-4" />
            </Button>
          </div>
        ),
      },
    ],
    [],
  )

  const apexNS = (r: RRSet) => r.type === "NS" && open && r.name === zone(open).name

  return (
    <>
      <PageHeader
        eyebrow="Networking"
        title="DNS"
        description="Zones served by this cloud's name servers. Delegate your domain to them at your registrar."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
              <RefreshCw className="size-4" /> Refresh
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)} disabled={!location}>
              <Plus className="size-4" /> Create zone
            </Button>
          </>
        }
      />
      {rows.length === 0 && !isLoading ? (
        <EmptyState icon={Globe} title="No DNS zones" hint="Create a zone for a domain you own." />
      ) : (
        <DataTable columns={columns} data={rows} isLoading={isLoading} />
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create zone</DialogTitle>
            <DialogDescription>A zone name can be held by one project only, and it cannot sit above or below another project's zone.</DialogDescription>
          </DialogHeader>
          <div className="grid gap-1">
            <Label htmlFor="dns-zone">Domain</Label>
            <Input id="dns-zone" value={name} onChange={(e) => setName(e.target.value)} placeholder="example.org" />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>Cancel</Button>
            <Button disabled={create.isPending || !name.trim()} onClick={() => create.mutate()}>
              {create.isPending ? "Creating…" : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={!!open}
        onOpenChange={(o) => {
          if (!o) {
            setOpen(null)
            setRecord(emptyRecord) // a draft for this zone must not be saved into the next one
          }
        }}
      >
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{open ? zone(open).name : ""}</DialogTitle>
            <DialogDescription>Saving a name and type replaces all its records.</DialogDescription>
          </DialogHeader>
          {records.isError ? (
            <p className="text-sm text-destructive">{(records.error as Error).message}</p>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-muted-foreground">
                  <th className="py-1">Name</th><th>Type</th><th>TTL</th><th>Content</th><th />
                </tr>
              </thead>
              <tbody>
                {(records.data ?? []).map((r) => (
                  <tr key={r.name + r.type} className="border-t align-top">
                    <td className="py-1 break-all">{r.name}</td>
                    <td>{r.type}</td>
                    <td>{r.ttl}</td>
                    <td className="break-all">{r.records.map((x) => x.content).join(", ")}</td>
                    <td className="text-right">
                      {apexNS(r) ? null : (
                        <Button variant="ghost" size="sm" aria-label={`Delete ${r.name} ${r.type}`} onClick={() => setDeletingRecord(r)}>
                          <Trash2 className="size-4" />
                        </Button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <div className="grid grid-cols-[1fr_6rem_6rem] gap-2">
            <div className="grid gap-1">
              <Label htmlFor="dns-name">Name</Label>
              <Input id="dns-name" value={record.name} onChange={(e) => setRecord({ ...record, name: e.target.value })} placeholder="www, or @ for the zone" />
            </div>
            <div className="grid gap-1">
              <Label>Type</Label>
              <Select value={record.type} onValueChange={(v) => setRecord({ ...record, type: v })}>
                <SelectTrigger aria-label="Record type"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {TYPES.map((t) => <SelectItem key={t} value={t}>{t}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1">
              <Label htmlFor="dns-ttl">TTL</Label>
              <Input id="dns-ttl" type="number" min={60} value={record.ttl} onChange={(e) => setRecord({ ...record, ttl: e.target.value })} />
            </div>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="dns-content">Content, one record per line</Label>
            <textarea
              id="dns-content"
              className="min-h-16 rounded-md border bg-transparent p-2 text-sm"
              value={record.content}
              onChange={(e) => setRecord({ ...record, content: e.target.value })}
            />
          </div>
          <DialogFooter>
            <Button disabled={save.isPending || !record.content.trim()} onClick={() => save.mutate()}>Save record</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deletingRecord} onOpenChange={(o) => !o && setDeletingRecord(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {deletingRecord?.type} records of {deletingRecord?.name}?</DialogTitle>
            <DialogDescription>
              All of them go, and this cannot be undone: {deletingRecord?.records.map((x) => x.content).join(", ")}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeletingRecord(null)}>Cancel</Button>
            <Button variant="destructive" disabled={removeRecord.isPending} onClick={() => removeRecord.mutate(deletingRecord!)}>Delete records</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {deleting ? zone(deleting).name : ""}?</DialogTitle>
            <DialogDescription>Every record in the zone is deleted and the domain stops resolving.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>Delete zone</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
