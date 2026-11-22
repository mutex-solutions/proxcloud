// TLS certificates of a project, which the tls and https listeners of its load balancers present (backend:
// internal/platform/project/cloud_lb_certs.go). A certificate is uploaded once, as a PEM chain and its PEM key; the
// key is stored encrypted and no call returns it. To replace one: upload the new one, name it on the listeners,
// delete the old one.
import { useId, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { Plus, RefreshCw, ShieldCheck, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { fmtDate } from "@/lib/format"
import { useProjectId } from "@/lib/hooks"

export type LBCertificate = {
  id: string; name: string; createdAt?: string
  subject: string; issuer: string; dnsNames: string[]; notBefore: string; notAfter: string
  fingerprint: string; keyType: string; chainLength: number
  usedBy: string[]; warnings?: string[]
}

export const certificatesKey = (pid: string) => ["lb-certificates", pid]
export const fetchCertificates = (pid: string) => apiFetch<LBCertificate[]>(`/project/${pid}/certificates`)

// What a certificate's dates mean today: "" while it has more than 30 days left.
export function certificateExpiry(c: Pick<LBCertificate, "notAfter" | "notBefore">, now = Date.now()): { kind: "error" | "warn" | ""; text: string } {
  const days = Math.floor((new Date(c.notAfter).getTime() - now) / 86_400_000)
  if (days < 0) return { kind: "error", text: "Expired" }
  if (new Date(c.notBefore).getTime() > now) return { kind: "warn", text: "Not valid yet" }
  if (days <= 30) return { kind: "warn", text: days === 0 ? "Expires today" : `Expires in ${days} day${days === 1 ? "" : "s"}` }
  return { kind: "", text: "" }
}

export function ExpiryNote({ cert }: { cert: Pick<LBCertificate, "notAfter" | "notBefore"> }) {
  const e = certificateExpiry(cert)
  if (!e.kind) return null
  return <span className={e.kind === "error" ? "text-xs font-medium text-destructive" : "text-xs font-medium text-warning-text"}>{e.text}</span>
}

export default function CertificatesPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const nameId = useId(), chainId = useId(), keyId = useId()
  const certs = useQuery({ queryKey: certificatesKey(pid), queryFn: () => fetchCertificates(pid), enabled: !!pid })
  const invalidate = () => void qc.invalidateQueries({ queryKey: certificatesKey(pid) })

  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [chain, setChain] = useState("")
  const [key, setKey] = useState("")
  const [toDelete, setToDelete] = useState<LBCertificate | null>(null)
  const close = () => { setOpen(false); setName(""); setChain(""); setKey("") }

  const upload = useMutation({
    mutationFn: () => apiFetch<LBCertificate>(`/project/${pid}/certificates`, { method: "POST", body: { name: name.trim(), certificate: chain, privateKey: key } }),
    onSuccess: (c) => {
      toast.success(`Certificate ${c.name} uploaded`)
      for (const w of c.warnings ?? []) toast.warning(w)
      close()
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const del = useMutation({
    mutationFn: (c: LBCertificate) => apiFetch(`/project/${pid}/certificates/${c.id}`, { method: "DELETE" }),
    onSuccess: () => { toast.success("Certificate deleted"); setToDelete(null); invalidate() },
    onError: (e: Error) => { toast.error(e.message); setToDelete(null) },
  })
  const readFile = (set: (v: string) => void) => (e: React.ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0]
    if (f) void f.text().then(set)
  }

  const columns = useMemo<ColumnDef<LBCertificate, any>[]>(
    () => [
      { id: "name", accessorFn: (c) => c.name, header: sortableHeader("Name"), cell: ({ getValue }) => <span className="font-medium">{getValue()}</span> },
      {
        id: "names", accessorFn: (c) => c.dnsNames.join(", "), header: "Valid for",
        cell: ({ row }) => <span className="text-sm">{row.original.dnsNames.length ? row.original.dnsNames.join(", ") : <span className="text-muted-foreground">No name ({row.original.subject})</span>}</span>,
      },
      {
        id: "expires", accessorFn: (c) => c.notAfter, header: sortableHeader("Expires"),
        cell: ({ row }) => <div className="grid"><span className="text-sm">{fmtDate(row.original.notAfter)}</span><ExpiryNote cert={row.original} /></div>,
      },
      { id: "key", accessorFn: (c) => c.keyType, header: "Key", cell: ({ getValue }) => <span className="text-sm text-muted-foreground">{getValue()}</span> },
      {
        id: "fingerprint", accessorFn: (c) => c.fingerprint, header: "SHA-256 fingerprint",
        cell: ({ getValue }) => <span className="font-mono text-xs" title={getValue()}>{String(getValue()).slice(0, 16)}…</span>,
      },
      {
        id: "usedBy", accessorFn: (c) => c.usedBy.join(", "), header: "Used by",
        cell: ({ row }) => <span className="text-sm text-muted-foreground">{row.original.usedBy.length ? row.original.usedBy.join(", ") : "No listener"}</span>,
      },
      {
        id: "actions", header: () => null, enableSorting: false,
        cell: ({ row }) => (
          <div className="flex justify-end">
            <Button variant="ghost" size="sm" onClick={() => setToDelete(row.original)} aria-label={`Delete certificate ${row.original.name}`}>
              <Trash2 aria-hidden="true" /> Delete
            </Button>
          </div>
        ),
      },
    ],
    [],
  )

  return (
    <>
      <PageHeader
        title="Certificates"
        eyebrow="Network"
        description="TLS certificates for the TLS and HTTPS listeners of your load balancers."
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => void certs.refetch()} disabled={certs.isFetching} aria-label="Refresh">
              <RefreshCw className={certs.isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
            <Button size="sm" onClick={() => setOpen(true)}><Plus className="size-4" /> Upload certificate</Button>
          </>
        }
      />
      {certs.data && certs.data.length === 0 ? (
        <EmptyState icon={ShieldCheck} title="No certificates" hint="Upload a certificate and its key, then name it on a TLS or HTTPS listener of a load balancer." />
      ) : (
        <DataTable columns={columns} data={certs.data ?? []} isLoading={certs.isLoading} error={certs.error as Error | null} pagination={false} />
      )}
      <p className="mt-4 text-xs text-muted-foreground" data-testid="cert-replace-note">
        A certificate does not change after its upload. To replace one before it expires, upload the new one, put it on the listeners in its
        place and delete the old one: the load balancer takes the change without closing open connections.
      </p>

      <Dialog open={open} onOpenChange={(o) => !o && close()}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>Upload certificate</DialogTitle>
            <DialogDescription>
              PEM text. The private key is stored encrypted, goes only to your load balancers, and is never shown again: keep your own copy.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-1">
              <Label htmlFor={nameId}>Name</Label>
              <Input id={nameId} value={name} maxLength={32} onChange={(e) => setName(e.target.value)} placeholder="shop-2027" />
              <p className="text-xs text-muted-foreground">Lowercase letters, digits and dashes.</p>
            </div>
            <div className="grid gap-1">
              <Label htmlFor={chainId}>Certificate chain</Label>
              <Textarea id={chainId} rows={6} className="font-mono text-xs" value={chain} onChange={(e) => setChain(e.target.value)}
                placeholder={"-----BEGIN CERTIFICATE-----\n(your server's certificate)\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\n(the intermediate that signed it)\n-----END CERTIFICATE-----"} />
              <Input type="file" accept=".pem,.crt,.cer,.txt" aria-label="Certificate chain file" onChange={readFile(setChain)} />
              <p className="text-xs text-muted-foreground">Your server's certificate first, then each intermediate (fullchain.pem). At most 5 certificates.</p>
            </div>
            <div className="grid gap-1">
              <Label htmlFor={keyId}>Private key</Label>
              <Textarea id={keyId} rows={4} className="font-mono text-xs" value={key} onChange={(e) => setKey(e.target.value)} placeholder={"-----BEGIN PRIVATE KEY-----\n…\n-----END PRIVATE KEY-----"} />
              <Input type="file" accept=".pem,.key,.txt" aria-label="Private key file" onChange={readFile(setKey)} />
              <p className="text-xs text-muted-foreground">RSA of 2048 bits or more, or ECDSA P-256 or P-384, without a passphrase.</p>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={close}>Cancel</Button>
            <Button onClick={() => upload.mutate()} disabled={!name.trim() || !chain.trim() || !key.trim() || upload.isPending}>
              {upload.isPending ? "Uploading…" : "Upload"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete certificate</DialogTitle>
            <DialogDescription>
              Delete "{toDelete?.name}" and its private key? A certificate that a listener still uses is not deleted.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setToDelete(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => toDelete && del.mutate(toDelete)} disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete"}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
