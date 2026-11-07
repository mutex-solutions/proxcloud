import { useEffect, useState } from "react"
import { toast } from "sonner"
import { ExternalLink, ShieldAlert } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import {
  useS3Keys, useBucketSettings, useBucketSettingsMutation, type BucketGrant, type CorsRule,
} from "@/lib/objectstore"

const GiB = 1024 ** 3

function bytesToGb(b: number): string {
  return b > 0 ? String(Math.round((b / GiB) * 100) / 100) : ""
}

// An empty quota field = unlimited; a non-empty one must parse to a finite number STRICTLY > 0. Zero (and
// negatives) are rejected because the backend treats maxSizeBytes/maxObjects <= 0 as "unlimited", so a
// literal 0 would silently mean unlimited rather than "block everything".
function quotaFieldValid(v: string): boolean {
  if (v.trim() === "") return true
  const n = Number(v)
  return Number.isFinite(n) && n > 0
}

// Max objects is an int64 server-side — a decimal like "1.5" would fail the JSON decode, so unlike the
// GB field (rounded to bytes) it must be a whole number.
function quotaObjectsValid(v: string): boolean {
  if (v.trim() === "") return true
  const n = Number(v)
  return Number.isInteger(n) && n > 0
}

function quotaFieldsValid(gb: string, objects: string): boolean {
  return quotaFieldValid(gb) && quotaObjectsValid(objects) && (gb.trim() !== "" || objects.trim() !== "")
}

type Props = {
  pid: string
  resourceId: string
  bucketName: string
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function BucketSettingsDialog({ pid, resourceId, bucketName, open, onOpenChange }: Props) {
  const { data: s, isLoading, isError, error } = useBucketSettings(pid, resourceId, open)
  const { data: keys } = useS3Keys(pid, open)
  const mut = useBucketSettingsMutation(pid, resourceId)

  const run = (action: string, data?: Record<string, unknown>, msg?: string) =>
    mut.mutate({ action, data }, {
      onSuccess: () => msg && toast.success(msg),
      onError: (e: Error) => toast.error(e.message),
    })

  // local edit state, seeded from the server once per open (see the effect below)
  const [quotaGb, setQuotaGb] = useState("")
  const [quotaObjects, setQuotaObjects] = useState("")
  const [indexDoc, setIndexDoc] = useState("index.html")
  const [errorDoc, setErrorDoc] = useState("")
  // null = untouched (show the live policy, Save disabled). A string = the user has edited it.
  const [policy, setPolicy] = useState<string | null>(null)
  const [grantKey, setGrantKey] = useState("")
  const [grantPerm, setGrantPerm] = useState<BucketGrant["permission"]>("READ")

  // Seed/reset the editable fields from the live settings ONCE per open per bucket: reopening (or a parent
  // that reuses this dialog for another bucket) must not show the previous edits, and a bucket with an
  // existing website config must show its real index/error documents, not the hard-coded defaults.
  // seededFor clears on close, so in-progress edits are never clobbered while the dialog is open.
  const [seededFor, setSeededFor] = useState<string | null>(null)
  useEffect(() => {
    if (!open) {
      setSeededFor(null)
      return
    }
    if (!s || seededFor === resourceId) return
    setQuotaGb("")
    setQuotaObjects("")
    setIndexDoc(s.website?.indexDocument || "index.html")
    setErrorDoc(s.website?.errorDocument || "")
    setPolicy(null)
    setGrantKey("")
    setGrantPerm("READ")
    setSeededFor(resourceId)
  }, [open, s, seededFor, resourceId])

  const websiteTab = s?.caps?.website !== false || !!s?.website?.enabled
  const keyByUid = new Map((keys ?? []).map((k) => [k.rgwUid, k]))
  const ungranted = (keys ?? []).filter((k) => !s?.grants?.some((g) => g.uid === k.rgwUid))

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Bucket settings — {bucketName}</DialogTitle>
          <DialogDescription>Configuration for this S3 bucket.</DialogDescription>
        </DialogHeader>

        {isLoading ? (
          <Skeleton className="h-72" />
        ) : isError ? (
          <p className="rounded-md bg-muted p-4 text-sm text-muted-foreground">{(error as Error).message}</p>
        ) : !s ? null : (
          // A live website stays closable where the provider no longer offers websites; the key returns to
          // General when closing it removes the tab.
          <Tabs key={websiteTab ? "site" : "no-site"} defaultValue="general">
            <TabsList>
              <TabsTrigger value="general">General</TabsTrigger>
              {websiteTab ? <TabsTrigger value="website">Website</TabsTrigger> : null}
              <TabsTrigger value="access">Access</TabsTrigger>
              <TabsTrigger value="lifecycle">Lifecycle</TabsTrigger>
              <TabsTrigger value="cors">CORS</TabsTrigger>
              <TabsTrigger value="policy">Policy</TabsTrigger>
            </TabsList>

            {/* --- general: versioning, object lock, quota, placement --- */}
            <TabsContent value="general" className="grid gap-5 pt-4">
              <div className="flex items-center justify-between gap-4">
                <div className="grid gap-1">
                  <Label className="text-eyebrow">Versioning</Label>
                  <p className="text-xs text-muted-foreground">
                    Keeps previous versions of every object. Old versions still count towards storage you are billed
                    for. Once enabled it can only be suspended, never fully turned off.
                  </p>
                </div>
                <Switch
                  checked={s.versioning === "Enabled"}
                  onCheckedChange={(v) => run("SET_VERSIONING", { enabled: v }, v ? "Versioning enabled" : "Versioning suspended")}
                />
              </div>

              <div className="grid gap-1.5">
                <Label className="text-eyebrow">Object lock</Label>
                {s.objectLock?.enabled ? (
                  <>
                    <p className="text-xs text-muted-foreground">
                      Enabled at creation. Retention: {s.objectLock.mode || "—"}{" "}
                      {s.objectLock.days ? `· ${s.objectLock.days} day(s)` : ""}
                    </p>
                    <div className="flex items-end gap-2">
                      <div className="grid gap-1">
                        <Label className="text-xs text-muted-foreground">Default retention (days)</Label>
                        <Input
                          type="number"
                          min={1}
                          defaultValue={s.objectLock.days || 1}
                          className="w-32"
                          id="lock-days"
                        />
                      </div>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => {
                          const el = document.getElementById("lock-days") as HTMLInputElement | null
                          // The backend expects an integer day count — reject decimals/NaN up front.
                          const days = Number(el?.value)
                          if (!Number.isInteger(days) || days < 1) {
                            toast.error("Retention must be a whole number of days (1 or more)")
                            return
                          }
                          run("SET_OBJECT_LOCK", { mode: "GOVERNANCE", days }, "Retention updated")
                        }}
                      >
                        Apply
                      </Button>
                    </div>
                    <p className="text-xs text-muted-foreground">
                      Only GOVERNANCE retention is offered. COMPLIANCE would make objects — and this project —
                      impossible to delete until retention expires.
                    </p>
                  </>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    Not enabled. Object lock can only be turned on when the bucket is created.
                  </p>
                )}
              </div>

              <div className="grid gap-1.5">
                <Label className="text-eyebrow">Quota</Label>
                <div className="flex flex-wrap items-end gap-2">
                  <div className="grid gap-1">
                    <Label className="text-xs text-muted-foreground">Max size (GB)</Label>
                    <Input
                      className="w-32"
                      placeholder={s.quota.enabled && s.quota.maxSizeBytes > 0 ? bytesToGb(s.quota.maxSizeBytes) : "unlimited"}
                      value={quotaGb}
                      onChange={(e) => setQuotaGb(e.target.value)}
                    />
                  </div>
                  {s.caps?.quotaObjects !== false ? (
                    <div className="grid gap-1">
                      <Label className="text-xs text-muted-foreground">Max objects</Label>
                      <Input
                        className="w-32"
                        placeholder={s.quota.enabled && s.quota.maxObjects > 0 ? String(s.quota.maxObjects) : "unlimited"}
                        value={quotaObjects}
                        onChange={(e) => setQuotaObjects(e.target.value)}
                      />
                    </div>
                  ) : null}
                  <Button
                    variant="outline"
                    size="sm"
                    // These are plain text inputs, so guard against NaN before sending — an empty field
                    // means "unlimited" (-1), anything else must be strictly > 0 (the backend reads <= 0
                    // as unlimited, so a literal 0 would not mean "block everything").
                    disabled={!quotaFieldsValid(quotaGb, quotaObjects)}
                    onClick={() =>
                      run(
                        "SET_QUOTA",
                        {
                          maxSizeBytes: quotaGb.trim() ? Math.round(Number(quotaGb) * GiB) : -1,
                          maxObjects: quotaObjects.trim() ? Number(quotaObjects) : -1,
                          enabled: true,
                        },
                        "Quota updated",
                      )
                    }
                  >
                    Apply
                  </Button>
                  {s.quota.enabled ? (
                    <Button variant="ghost" size="sm" onClick={() => run("SET_QUOTA", { maxSizeBytes: -1, maxObjects: -1, enabled: false }, "Quota removed")}>
                      Remove quota
                    </Button>
                  ) : null}
                </div>
                <p className="text-xs text-muted-foreground">Leave a field empty for unlimited.</p>
              </div>

              <div className="flex gap-6 text-xs text-muted-foreground">
                <span>Index type: {s.indexType || "—"}</span>
                <span>Placement: {s.placementRule || "—"}</span>
              </div>
            </TabsContent>

            {/* --- website --- */}
            <TabsContent value="website" className="grid gap-4 pt-4">
              {s.website?.enabled ? (
                <Alert variant="destructive">
                  <ShieldAlert className="size-4" />
                  <AlertTitle>This bucket is public</AlertTitle>
                  <AlertDescription>
                    Every object in <strong>{bucketName}</strong> can be read by anyone on the internet. Turning the
                    website off removes public access again.
                  </AlertDescription>
                </Alert>
              ) : (
                <Alert>
                  <ShieldAlert className="size-4" />
                  <AlertTitle>Enabling a website makes every object public</AlertTitle>
                  <AlertDescription>
                    ProxCloud adds a bucket policy allowing anonymous read of all objects. Do not enable it on a bucket
                    holding private data.
                  </AlertDescription>
                </Alert>
              )}

              {s.website?.enabled && s.website.url ? (
                <div className="grid gap-1.5">
                  <Label className="text-xs text-muted-foreground">Website URL</Label>
                  <a
                    className="flex items-center gap-1 text-sm text-primary-text hover:underline"
                    href={s.website.url}
                    target="_blank"
                    rel="noreferrer"
                  >
                    {s.website.url} <ExternalLink className="size-3" />
                  </a>
                </div>
              ) : null}

              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div className="grid gap-1.5">
                  <Label htmlFor="idx">Index document</Label>
                  <Input id="idx" value={indexDoc} onChange={(e) => setIndexDoc(e.target.value)} />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="err">Error document (optional)</Label>
                  <Input id="err" placeholder="error.html" value={errorDoc} onChange={(e) => setErrorDoc(e.target.value)} />
                </div>
              </div>

              {s.website?.enabled ? (
                <Button variant="destructive" onClick={() => run("DISABLE_WEBSITE", undefined, "Website disabled; public access removed")}>
                  Disable website
                </Button>
              ) : (
                <Button
                  onClick={() =>
                    run("ENABLE_WEBSITE", { indexDocument: indexDoc, errorDocument: errorDoc }, "Website enabled — bucket is now public")
                  }
                >
                  Enable website (makes objects public)
                </Button>
              )}
            </TabsContent>

            {/* --- access: per-key grants --- */}
            <TabsContent value="access" className="grid gap-4 pt-4">
              <p className="text-xs text-muted-foreground">
                Grant one of this project’s S3 access keys permission on this bucket. The project’s own credentials
                always have full access.
              </p>

              {s.grants.length ? (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Key</TableHead>
                      <TableHead>Permission</TableHead>
                      <TableHead className="w-24" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {s.grants.map((g) => (
                      <TableRow key={g.uid}>
                        <TableCell className="font-medium">{keyByUid.get(g.uid)?.name ?? g.uid}</TableCell>
                        <TableCell>
                          <Badge variant="secondary">{g.permission.replace("_", " ")}</Badge>
                        </TableCell>
                        <TableCell>
                          <Button
                            variant="ghost"
                            size="sm"
                            className="text-destructive"
                            onClick={() => {
                              const k = keyByUid.get(g.uid)
                              if (k) run("REVOKE_KEY", { keyId: k.id }, "Access revoked")
                              else toast.error("This grant refers to a key that no longer exists")
                            }}
                          >
                            Revoke
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <p className="text-sm text-muted-foreground">No keys have access to this bucket.</p>
              )}

              <div className="flex flex-wrap items-end gap-2">
                <div className="grid gap-1">
                  <Label className="text-xs text-muted-foreground">Access key</Label>
                  <Select value={grantKey} onValueChange={setGrantKey}>
                    <SelectTrigger className="w-56">
                      <SelectValue placeholder={ungranted.length ? "Select a key" : "No keys available"} />
                    </SelectTrigger>
                    <SelectContent>
                      {ungranted.map((k) => (
                        <SelectItem key={k.id} value={k.id}>{k.name}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="grid gap-1">
                  <Label className="text-xs text-muted-foreground">Permission</Label>
                  <Select value={grantPerm} onValueChange={(v) => setGrantPerm(v as BucketGrant["permission"])}>
                    <SelectTrigger className="w-44">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="READ">Read</SelectItem>
                      <SelectItem value="READ_WRITE">Read &amp; write</SelectItem>
                      <SelectItem value="FULL">Full control</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!grantKey}
                  onClick={() => {
                    run("GRANT_KEY", { keyId: grantKey, permission: grantPerm }, "Access granted")
                    setGrantKey("")
                  }}
                >
                  Grant
                </Button>
              </div>
            </TabsContent>

            {/* --- lifecycle --- */}
            <TabsContent value="lifecycle" className="grid gap-4 pt-4">
              <p className="text-xs text-muted-foreground">
                Automatically delete objects after a number of days. Useful for logs and temporary files — expired
                objects stop being billed.
              </p>
              {s.lifecycle.length ? (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Rule</TableHead>
                      <TableHead>Prefix</TableHead>
                      <TableHead>Expire after</TableHead>
                      <TableHead className="w-24" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {s.lifecycle.map((r) => (
                      <TableRow key={r.id}>
                        <TableCell className="font-medium">{r.id}</TableCell>
                        <TableCell className="font-mono text-xs">{r.prefix || "(all objects)"}</TableCell>
                        <TableCell>{r.expirationDays ? `${r.expirationDays} days` : "—"}</TableCell>
                        <TableCell>
                          <Button
                            variant="ghost"
                            size="sm"
                            className="text-destructive"
                            onClick={() =>
                              run("SET_LIFECYCLE", { rules: s.lifecycle.filter((x) => x.id !== r.id) }, "Rule removed")
                            }
                          >
                            Remove
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <p className="text-sm text-muted-foreground">No lifecycle rules.</p>
              )}
              <LifecycleAdd
                onAdd={(rule) => run("SET_LIFECYCLE", { rules: [...s.lifecycle, rule] }, "Rule added")}
                existing={s.lifecycle.map((r) => r.id)}
              />
            </TabsContent>

            {/* --- CORS --- */}
            <TabsContent value="cors" className="grid gap-4 pt-4">
              <p className="text-xs text-muted-foreground">
                Allow web apps on other origins to call this bucket from the browser. Without a matching rule,
                cross-origin downloads and uploads are blocked by the browser — direct links still work.
              </p>
              {s.cors.length ? (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Origins</TableHead>
                      <TableHead>Methods</TableHead>
                      <TableHead>Max age</TableHead>
                      <TableHead className="w-24" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {s.cors.map((r, i) => (
                      <TableRow key={i}>
                        <TableCell>
                          <div className="font-mono text-xs">{r.allowedOrigins.join(", ")}</div>
                          {r.allowedHeaders?.length || r.exposeHeaders?.length ? (
                            <div className="mt-1 text-xs text-muted-foreground">
                              {r.allowedHeaders?.length ? `Headers: ${r.allowedHeaders.join(", ")}` : ""}
                              {r.allowedHeaders?.length && r.exposeHeaders?.length ? " · " : ""}
                              {r.exposeHeaders?.length ? `Expose: ${r.exposeHeaders.join(", ")}` : ""}
                            </div>
                          ) : null}
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-wrap gap-1">
                            {r.allowedMethods.map((m) => (
                              <Badge key={m} variant="secondary">{m}</Badge>
                            ))}
                          </div>
                        </TableCell>
                        <TableCell>{r.maxAgeSeconds ? `${r.maxAgeSeconds}s` : "—"}</TableCell>
                        <TableCell>
                          <Button
                            variant="ghost"
                            size="sm"
                            className="text-destructive"
                            onClick={() =>
                              run("SET_CORS", { rules: s.cors.filter((_, j) => j !== i) }, "CORS rule removed")
                            }
                          >
                            Remove
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <p className="text-sm text-muted-foreground">No CORS rules.</p>
              )}
              <CorsAdd onAdd={(rule) => run("SET_CORS", { rules: [...s.cors, rule] }, "CORS rule added")} />
            </TabsContent>

            {/* --- raw policy --- */}
            <TabsContent value="policy" className="grid gap-3 pt-4">
              <p className="text-xs text-muted-foreground">
                Advanced. ProxCloud-managed statements (website public-read and key grants) are preserved automatically
                and cannot be edited here.
              </p>
              <Textarea
                rows={12}
                className="font-mono text-xs"
                placeholder="No policy set"
                // Seeded from the LIVE policy. If this were only a placeholder, pressing Save without
                // retyping would submit "" and wipe the customer's statements.
                value={policy ?? s.policyJson ?? ""}
                onChange={(e) => setPolicy(e.target.value)}
              />
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={policy === null}
                  onClick={() => run("SET_POLICY", { policyJson: policy ?? "" }, "Policy saved")}
                >
                  Save policy
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    setPolicy("")
                    run("SET_POLICY", { policyJson: "" }, "Custom policy cleared")
                  }}
                >
                  Clear custom policy
                </Button>
              </div>
              {policy === null ? (
                <p className="text-xs text-muted-foreground">Edit the document to enable Save.</p>
              ) : null}
            </TabsContent>
          </Tabs>
        )}
      </DialogContent>
    </Dialog>
  )
}

const CORS_METHODS = ["GET", "HEAD", "PUT", "POST", "DELETE"] as const

// Comma-separated input → trimmed non-empty list.
function csvList(v: string): string[] {
  return v.split(",").map((x) => x.trim()).filter(Boolean)
}

function CorsAdd({ onAdd }: { onAdd: (r: CorsRule) => void }) {
  const [origins, setOrigins] = useState("")
  const [methods, setMethods] = useState<string[]>(["GET", "HEAD"])
  const [headers, setHeaders] = useState("")
  const [expose, setExpose] = useState("")
  const [maxAge, setMaxAge] = useState("")
  // maxAgeSeconds is an int32 server-side — a decimal would fail the JSON decode.
  const maxAgeNum = Number(maxAge)
  const maxAgeValid = maxAge.trim() === "" || (Number.isInteger(maxAgeNum) && maxAgeNum >= 0)
  const originList = csvList(origins)
  return (
    <div className="grid gap-3">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="grid gap-1">
          <Label className="text-xs text-muted-foreground">Allowed origins (comma-separated)</Label>
          <Input
            value={origins}
            onChange={(e) => setOrigins(e.target.value)}
            placeholder="https://app.example.com, *"
          />
        </div>
        <div className="grid gap-1">
          <Label className="text-xs text-muted-foreground">Methods</Label>
          <div className="flex flex-wrap gap-3 pt-1.5">
            {CORS_METHODS.map((m) => (
              <label key={m} className="flex cursor-pointer items-center gap-1.5 text-sm">
                <Checkbox
                  checked={methods.includes(m)}
                  onCheckedChange={(v) =>
                    setMethods((ms) => (v === true ? [...ms, m] : ms.filter((x) => x !== m)))
                  }
                />
                {m}
              </label>
            ))}
          </div>
        </div>
        <div className="grid gap-1">
          <Label className="text-xs text-muted-foreground">Allowed headers (optional)</Label>
          <Input value={headers} onChange={(e) => setHeaders(e.target.value)} placeholder="*" />
        </div>
        <div className="grid gap-1">
          <Label className="text-xs text-muted-foreground">Expose headers (optional)</Label>
          <Input value={expose} onChange={(e) => setExpose(e.target.value)} placeholder="ETag, Content-Length" />
        </div>
      </div>
      <div className="flex items-end gap-2">
        <div className="grid gap-1">
          <Label className="text-xs text-muted-foreground">Max age (seconds, optional)</Label>
          <Input
            className="w-40"
            type="number"
            min={0}
            value={maxAge}
            onChange={(e) => setMaxAge(e.target.value)}
            placeholder="3600"
          />
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={originList.length === 0 || methods.length === 0 || !maxAgeValid}
          onClick={() => {
            onAdd({
              allowedOrigins: originList,
              // canonical method order regardless of click order
              allowedMethods: CORS_METHODS.filter((m) => methods.includes(m)),
              ...(csvList(headers).length ? { allowedHeaders: csvList(headers) } : {}),
              ...(csvList(expose).length ? { exposeHeaders: csvList(expose) } : {}),
              ...(maxAge.trim() ? { maxAgeSeconds: maxAgeNum } : {}),
            })
            setOrigins("")
            setHeaders("")
            setExpose("")
            setMaxAge("")
          }}
        >
          Add rule
        </Button>
      </div>
    </div>
  )
}

function LifecycleAdd({ onAdd, existing }: { onAdd: (r: Record<string, unknown>) => void; existing: string[] }) {
  const [id, setId] = useState("")
  const [prefix, setPrefix] = useState("")
  const [days, setDays] = useState("30")
  const dup = existing.includes(id.trim())
  // expirationDays is an integer server-side — reject decimals/NaN, not just < 1.
  const dayCount = Number(days)
  const daysValid = Number.isInteger(dayCount) && dayCount >= 1
  return (
    <div className="flex flex-wrap items-end gap-2">
      <div className="grid gap-1">
        <Label className="text-xs text-muted-foreground">Rule name</Label>
        <Input className="w-40" value={id} onChange={(e) => setId(e.target.value)} placeholder="expire-logs" />
      </div>
      <div className="grid gap-1">
        <Label className="text-xs text-muted-foreground">Prefix (optional)</Label>
        <Input className="w-40" value={prefix} onChange={(e) => setPrefix(e.target.value)} placeholder="logs/" />
      </div>
      <div className="grid gap-1">
        <Label className="text-xs text-muted-foreground">Expire after (days)</Label>
        <Input className="w-32" type="number" min={1} value={days} onChange={(e) => setDays(e.target.value)} />
      </div>
      <Button
        variant="outline"
        size="sm"
        disabled={!id.trim() || dup || !daysValid}
        onClick={() => {
          onAdd({ id: id.trim(), prefix, enabled: true, expirationDays: dayCount })
          setId("")
          setPrefix("")
        }}
      >
        Add rule
      </Button>
      {dup ? <span className="text-xs text-destructive">Name already used</span> : null}
    </div>
  )
}
