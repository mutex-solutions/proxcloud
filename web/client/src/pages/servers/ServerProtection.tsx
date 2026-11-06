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
  target: string
  rpoTargetMinutes: number
  lastSync: number
  failCount: number
  error?: string
}
type Result = { available: boolean; reason?: string; protection?: Protection | null }

const LABEL = { syncing: "First sync running", active: "Protected", degraded: "Protected, replication behind" }

// The add-on: a copy of the disks on a second node and automatic restart there if this node fails.
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
      toast.success(r.protection ? "Protection is starting: the first sync can take a while" : "Protection removed")
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
          Keeps a copy of this server's disks on a second host. If its host fails, the server restarts there, with its
          data as of the last sync: anything written after that is lost. Billed per replicated GB once the first sync is
          done.
        </p>
        {p ? (
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1">
            <dt className="text-muted-foreground">Status</dt>
            <dd>{LABEL[p.state]}</dd>
            <dt className="text-muted-foreground">Copy on</dt>
            <dd>{p.target}</dd>
            <dt className="text-muted-foreground">Target data loss</dt>
            <dd>at most {p.rpoTargetMinutes} minutes</dd>
            <dt className="text-muted-foreground">Last sync</dt>
            <dd>{p.lastSync ? timeAgo(new Date(p.lastSync * 1000)) : "not yet"}</dd>
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
