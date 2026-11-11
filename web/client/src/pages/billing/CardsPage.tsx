import { useEffect, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { CreditCard as CreditCardIcon, MoreHorizontal, Plus, Star, Trash2 } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { DataTable, sortableHeader } from "@/components/data-table"
import { EmptyState } from "@/components/empty-state"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtMoney } from "@/lib/format"
import { useBillingSummary, useProjectId } from "@/lib/hooks"
import type { CreditCard, Gateway } from "@/lib/types"
import {
  GatewaySelect, cardGateways, defaultGatewayId, errMsg, gatewaySlug, useCardElement,
} from "./NewCardDepositDialog"

type AddCardResponse = {
  transactionId?: string
  externalPaymentId?: string
  thirdParty?: string
  metadata?: { client_secret?: string; customerId?: string; currency?: string; verificationAmount?: string }
}

export default function CardsPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { data: summary } = useBillingSummary(pid)
  const bp = summary?.id
  const defaultCardId = summary?.defaultCardId

  const { data: cards, isLoading, error: cardsError } = useQuery({
    queryKey: ["cards", bp],
    queryFn: () => apiFetch<CreditCard[]>(`/card/${bp}`),
    enabled: !!bp,
  })
  const { data: gateways } = useQuery({
    queryKey: ["payment-gateways", bp],
    queryFn: () => apiFetch<Gateway[]>(`/payment/${bp}/gateway`),
    enabled: !!bp,
  })
  const cardGws = cardGateways(gateways, "addCard")

  const [addOpen, setAddOpen] = useState(false)
  const [deleting, setDeleting] = useState<CreditCard | null>(null)

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["cards", bp] })
    void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })
  }

  const setDefault = useMutation({
    // POST /card/{bp}/{cardId}/default → updated billing summary (defaultCardId).
    mutationFn: (cardId: string) => apiFetch(`/card/${bp}/${cardId}/default`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Default card updated")
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const remove = useMutation({
    // DELETE /card/{cardId} — the route reuses the {billingProfileId} param slot for the CARD id.
    mutationFn: (cardId: string) => apiFetch(`/card/${cardId}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Card deleted")
      setDeleting(null)
      invalidate()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const setDefaultPending = setDefault.isPending
  const columns = useMemo<ColumnDef<CreditCard, any>[]>(
    () => [
      {
        id: "card",
        accessorFn: (c) => c.panMasked ?? c.id,
        header: sortableHeader("Card"),
        cell: ({ row }) => {
          const c = row.original
          return (
            <span className="flex items-center gap-3">
              <span className="flex h-7 w-10 shrink-0 items-center justify-center rounded-md border bg-muted/50">
                <CreditCardIcon className="size-4 text-muted-foreground" aria-hidden="true" />
              </span>
              <span className="font-mono text-sm tracking-wider tabular-nums">{c.panMasked ?? c.id}</span>
              {c.id === defaultCardId ? <Badge variant="secondary">Default</Badge> : null}
            </span>
          )
        },
      },
      {
        id: "expires",
        accessorFn: (c) => c.tokenExpirationDate ?? "",
        header: sortableHeader("Expires"),
        cell: ({ getValue }) => (
          <span className="text-sm text-muted-foreground tabular-nums">{fmtDate(getValue())}</span>
        ),
      },
      {
        id: "added",
        accessorFn: (c) => (c.createdAt as string | undefined) ?? "",
        header: sortableHeader("Added"),
        cell: ({ getValue }) => (
          <span className="text-sm text-muted-foreground tabular-nums">{fmtDate(getValue())}</span>
        ),
      },
      {
        id: "actions",
        header: () => null,
        enableSorting: false,
        cell: ({ row }) => {
          const c = row.original
          return (
            <div className="text-right" onClick={(e) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon-sm" aria-label={`Actions for card ${c.panMasked ?? c.id}`}>
                    <MoreHorizontal className="size-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem
                    onClick={() => setDefault.mutate(c.id)}
                    disabled={c.id === defaultCardId || setDefaultPending}
                  >
                    <Star className="size-4" /> Set as default
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setDeleting(c)}>
                    <Trash2 className="size-4" /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          )
        },
      },
    ],
    // setDefault.mutate and setDeleting are stable; the cells re-render off these two values.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [defaultCardId, setDefaultPending],
  )

  return (
    <>
      <PageHeader
        title="Cards"
        eyebrow="Billing"
        description="Saved payment cards on this billing profile."
        actions={
          <Button size="sm" onClick={() => setAddOpen(true)} disabled={!bp || !cardGws.length}>
            <Plus className="size-4" /> Add card
          </Button>
        }
      />

      {isLoading || !bp ? (
        <Skeleton className="h-64" />
      ) : !cardsError && !cards?.length ? (
        <EmptyState
          icon={CreditCardIcon}
          title="No cards yet"
          hint={`Add a card to enable deposits and automatic bill collection. To verify it we place a temporary ${fmtMoney(1, summary?.currency)} hold that is released immediately — nothing is charged.`}
          action={
            <Button onClick={() => setAddOpen(true)} disabled={!cardGws.length}>
              <Plus className="size-4" /> Add card
            </Button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={cards}
          error={cardsError as Error | null}
          getRowId={(c) => c.id}
        />
      )}

      {bp && cardGws.length ? (
        <AddCardDialog
          open={addOpen}
          onOpenChange={setAddOpen}
          bp={bp}
          gateways={cardGws}
          preferredGatewayId={summary?.paymentGatewayId}
          currency={summary?.currency}
          onAdded={invalidate}
        />
      ) : null}

      <Dialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete card</DialogTitle>
            <DialogDescription>
              Delete card <span className="font-mono">{deleting?.panMasked ?? deleting?.id}</span>? This cannot
              be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => deleting && remove.mutate(deleting.id)}
              disabled={remove.isPending}
            >
              {remove.isPending ? "Deleting…" : "Delete card"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

// Add-card flow ("like GCP"): POST /card/{bp}/add {paymentGatewayId} → a manual-capture 1.00
// verification PaymentIntent (client_secret + customer) → confirm it with the gateway's card
// element (Stripe confirmCardPayment / Airwallex card.confirm with a merchant-initiated consent)
// → GET the card-confirm callback so the API stores the card and releases the hold.
function AddCardDialog({
  open, onOpenChange, bp, gateways, preferredGatewayId, currency, onAdded,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  bp: string
  gateways: Gateway[]
  preferredGatewayId?: string
  currency?: string
  onAdded: () => void
}) {
  const [gwId, setGwId] = useState(() => defaultGatewayId(gateways, preferredGatewayId))
  const gateway = gateways.find((g) => g.id === gwId)
  const el = useCardElement(gateway, open)
  const [setup, setSetup] = useState<{
    txnId: string; intentId: string; clientSecret: string; customerId?: string; currency?: string; verificationAmount?: string
  } | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    setSetup(null)
    setError(null)
    if (!open) {
      setGwId(defaultGatewayId(gateways, preferredGatewayId))
      return
    }
    if (!gwId) return
    let cancelled = false
    ;(async () => {
      try {
        const resp = await apiFetch<AddCardResponse>(`/card/${bp}/add`, {
          method: "POST",
          body: { paymentGatewayId: gwId },
        })
        const md = resp?.metadata
        if (!resp?.transactionId || !md?.client_secret) throw new Error("Gateway did not return a verification secret")
        if (cancelled) return
        setSetup({
          txnId: resp.transactionId, intentId: resp.externalPaymentId ?? "", clientSecret: md.client_secret,
          customerId: md.customerId, currency: md.currency, verificationAmount: md.verificationAmount,
        })
      } catch (e) {
        if (!cancelled) setError(errMsg(e))
      }
    })()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, bp, gwId])

  const confirm = async () => {
    if (!setup) return
    setSaving(true)
    setError(null)
    try {
      await el.confirm({
        clientSecret: setup.clientSecret,
        intentId: setup.intentId,
        // Airwallex only: save the card for merchant-initiated (unscheduled) charges, hold only.
        airwallex: {
          customer_id: setup.customerId,
          payment_method_options: { card: { auto_capture: false } },
          payment_consent: { next_triggered_by: "merchant", merchant_trigger_reason: "unscheduled" },
        },
      })
      // Finalize server-side (whitelisted callback: retrieves the intent, stores the card and
      // releases the hold; throws here if the server couldn't finalize).
      await apiFetch(`/callbacks/payment/${gatewaySlug(gateway)}/card/confirm/${setup.txnId}`)
      toast.success("Card added")
      onAdded()
      onOpenChange(false)
    } catch (e) {
      setError(errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  const hold = fmtMoney(setup?.verificationAmount ?? 1, setup?.currency ?? currency)
  const shownError = error ?? el.error
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add card</DialogTitle>
          <DialogDescription>
            To verify the card we place a temporary {hold} hold that is released immediately — nothing is
            charged. Card details go directly to {gateway?.thirdParty ?? "the payment gateway"} — they never touch ProxCloud.
          </DialogDescription>
        </DialogHeader>
        <GatewaySelect gateways={gateways} value={gwId} onChange={setGwId} disabled={saving} />
        <div className="rounded-md border bg-muted/30 p-3">
          <div ref={el.mountRef} />
        </div>
        {shownError ? <p className="text-sm text-destructive">{shownError}</p> : null}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={() => void confirm()} disabled={!setup || !el.ready || saving}>
            {saving ? "Saving…" : "Save card"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
