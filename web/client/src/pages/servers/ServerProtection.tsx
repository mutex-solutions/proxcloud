import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { ShieldCheck, ShieldOff } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { apiFetch } from "@/lib/api"
import { timeAgo } from "@/lib/format"

type Protection = {
  state: "syncing" | "active" | "degraded"
  mode?: "shared"
  target: string
  rpoTargetMinutes: number
  lastSync: number
  failCount: number
  error?: string
}
type Result = { available: boolean; reason?: string; protection?: Protection | null }

const LABEL = { syncing: "First sync running", active: "Protected", degraded: "Protected, replication behind" }

// The add-on: automatic restart on another node if this node fails, with a copy of the disks there unless they are on
// shared storage.
export function ServerProtection({ pid, resourceId }: { pid: string; resourceId: string }) {
  const qc = useQueryClient()
  const key = ["server-protection", pid, resourceId]
  const action = (name: string) =>
    apiFetch<{ result: Result }>(`/project/${pid}/cloud/${resourceId}/action`, { method: "POST", body: { action: name } }).then((r) => r.result)
  const { data, isLoading, error } = useQuery({
    queryKey: key,
    queryFn: () => action("GET_PROTECTION"),
    refetchInterval: (q) => (q.state.data?.protection?.state === "syncing" ? 5000 : 60000),
  })
  const change = useMutation({
    mutationFn: action,
    onSuccess: (r) => {
      qc.setQueryData(key, r)
      if (!r.protection) toast.success("Protection removed")
      else if (r.protection.mode === "shared") toast.success("Protection is on")
      else toast.success("Protection is starting: the first sync can take a while")
    },
    onError: (e: Error) => toast.error(e.message),
  })
  if (isLoading) return null
  if (error) return <p className="text-sm text-muted-foreground">{(error as Error).message}</p>
  const p = data?.protection
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-eyebrow">Protection</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4 text-sm">
        <p className="text-muted-foreground">
          If this server's host fails, the server restarts on another one. On local disks that takes a copy on a second
          host, with the data as of the last sync: anything written after that is lost, and it is billed per replicated
          GB once the first sync is done. On shared storage every host already reaches the disks: nothing is copied, the
          restart is like a power cut, and there is no charge.
        </p>
        {p ? (
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1">
            <dt className="text-muted-foreground">Status</dt>
            <dd>{LABEL[p.state]}</dd>
            {p.mode === "shared" ? (
              <>
                <dt className="text-muted-foreground">Disks</dt>
                <dd>On shared storage, no copy needed</dd>
              </>
            ) : (
              <>
                <dt className="text-muted-foreground">Copy on</dt>
                <dd>{p.target}</dd>
                <dt className="text-muted-foreground">Target data loss</dt>
                <dd>at most {p.rpoTargetMinutes} minutes</dd>
                <dt className="text-muted-foreground">Last sync</dt>
                <dd>{p.lastSync ? timeAgo(new Date(p.lastSync * 1000)) : "not yet"}</dd>
              </>
            )}
          </dl>
        ) : null}
        {p?.error ? (
          <Alert variant="destructive">
            <AlertTitle>The last sync failed</AlertTitle>
            <AlertDescription>{p.error}</AlertDescription>
          </Alert>
        ) : null}
        {!p && data && !data.available ? <p className="text-muted-foreground">{data.reason}</p> : null}
        <div>
          {p ? (
            <Button variant="outline" size="sm" disabled={change.isPending} onClick={() => change.mutate("DISABLE_PROTECTION")}>
              <ShieldOff className="size-4" /> Remove protection
            </Button>
          ) : (
            <Button size="sm" disabled={change.isPending || !data?.available} onClick={() => change.mutate("ENABLE_PROTECTION")}>
              <ShieldCheck className="size-4" /> Enable protection
            </Button>
          )}
        </div>
      </CardContent>
    </Card>
  )
}
