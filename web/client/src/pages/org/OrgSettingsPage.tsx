import { useEffect, useState } from "react"
import { useNavigate } from "react-router-dom"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { useProjectId } from "@/lib/hooks"
import { useOrg } from "./MembersPage"

export default function OrgSettingsPage() {
  const pid = useProjectId()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const { org, isLoading, error } = useOrg(pid)

  const [name, setName] = useState("")
  const [description, setDescription] = useState("")
  // Prefill once the org loads (org.description may be absent — NON_NULL DTO).
  useEffect(() => {
    if (org) {
      setName(org.name ?? "")
      setDescription((org as { description?: string }).description ?? "")
    }
  }, [org?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  const save = useMutation({
    // PUT /organizations/{id} {name, description} — the Go handler reads exactly these two fields.
    mutationFn: () =>
      apiFetch(`/organizations/${org?.id}`, {
        method: "PUT",
        body: { name: name.trim(), description },
      }),
    onSuccess: () => {
      toast.success("Organization updated")
      void qc.invalidateQueries({ queryKey: ["organizations"] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const [deleteOpen, setDeleteOpen] = useState(false)
  const [confirmName, setConfirmName] = useState("")
  const remove = useMutation({
    mutationFn: () => apiFetch(`/organizations/${org?.id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Organization deleted")
      void qc.invalidateQueries({ queryKey: ["organizations"] })
      void qc.invalidateQueries({ queryKey: ["projects"] })
      navigate("/")
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <>
      <PageHeader
        title="Organization settings"
        eyebrow="Organization"
        description="Details and the danger zone for this organization."
      />

      {isLoading ? (
        <div className="max-w-2xl space-y-6">
          <Skeleton className="h-64" />
          <Skeleton className="h-28" />
        </div>
      ) : error ? (
        <div className="rounded-md border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(error as Error).message}
        </div>
      ) : !org ? (
        <div className="rounded-md border bg-muted/40 p-4 text-sm text-muted-foreground">
          No organization found.
        </div>
      ) : (
        <div className="max-w-2xl space-y-6">
          {/* Settings section: eyebrow-labelled card, mono identifiers. */}
          <Card className="gap-0 overflow-hidden py-0">
            <div className="text-eyebrow border-b px-5 py-3">Details</div>
            <div className="grid gap-4 p-5">
              <div>
                <Label className="mb-1.5 block">Organization ID</Label>
                <p className="font-mono text-xs text-muted-foreground">{org.id}</p>
              </div>
              <div>
                <Label className="mb-1.5 block" htmlFor="org-name">Name</Label>
                <Input id="org-name" value={name} onChange={(e) => setName(e.target.value)} />
              </div>
              <div>
                <Label className="mb-1.5 block" htmlFor="org-description">Description</Label>
                <Input
                  id="org-description"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  placeholder="Optional description"
                />
              </div>
              <div>
                <Button onClick={() => save.mutate()} disabled={!name.trim() || save.isPending}>
                  {save.isPending ? "Saving…" : "Save changes"}
                </Button>
              </div>
            </div>
          </Card>

          {/* Danger zone: destructive hairline border carries the signal. */}
          <Card className="gap-0 overflow-hidden border-destructive/40 py-0">
            <div className="text-eyebrow border-b border-destructive/40 px-5 py-3">Danger zone</div>
            <div className="flex flex-wrap items-center justify-between gap-3 p-5">
              <p className="max-w-sm text-sm text-muted-foreground">
                Deleting the organization removes it along with its membership and settings. This cannot be undone.
              </p>
              <Button
                variant="destructive"
                onClick={() => {
                  setConfirmName("")
                  setDeleteOpen(true)
                }}
              >
                <Trash2 className="size-4" /> Delete organization
              </Button>
            </div>
          </Card>
        </div>
      )}

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete organization</DialogTitle>
            <DialogDescription>
              This permanently deletes <span className="font-medium">{org?.name}</span>. Type the organization
              name to confirm.
            </DialogDescription>
          </DialogHeader>
          <div>
            <Label className="mb-1.5 block" htmlFor="org-delete-confirm">Organization name</Label>
            <Input
              id="org-delete-confirm"
              autoComplete="off"
              spellCheck={false}
              value={confirmName}
              onChange={(e) => setConfirmName(e.target.value)}
              placeholder={org?.name}
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => remove.mutate()}
              disabled={confirmName !== (org?.name ?? "") || remove.isPending}
            >
              {remove.isPending ? "Deleting…" : "Delete organization"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
