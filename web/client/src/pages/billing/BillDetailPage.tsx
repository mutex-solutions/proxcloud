import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { CreditCard as CreditCardIcon, Download, Landmark, Wallet } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatusBadge } from "@/components/status-badge"
import {
  Breadcrumb, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtDateTime, fmtMoney } from "@/lib/format"
import { useBillingSummary, useProjectId, useProjects } from "@/lib/hooks"
import type { Bill, CreditCard, Gateway, Transaction } from "@/lib/types"
import { cn } from "@/lib/utils"
import { downloadPdf } from "./HistoryPage"
import {
  BankTransferPanel, PendingBankTransfers, transferFromDeposit, type DepositResponse,
} from "./BankTransferPanel"
import { NewCardDepositDialog, cardGateways } from "./NewCardDepositDialog"

// POST /payment/{bp}/bill/{billId}/collect result (CollectTransaction DTO).
type CollectResult = { id?: string; status?: string; errorMessage?: string; grossAmount?: number; currency?: string }

export default function BillDetailPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { billId = "" } = useParams()
  const { data: summary } = useBillingSummary(pid)
  const bp = summary?.id
  const [payOpen, setPayOpen] = useState(false)
  // The bill is org (billing-profile) scoped — all projects' line items. Resolve + filter by project
  // so an org admin can read per-project spend on the same bill.
  const { data: projects } = useProjects()
  const [projFilter, setProjFilter] = useState("__all__")
  // An id not in the caller's project list is a deleted project, or a live one this member cannot
  // see (the org bill is wider than their projects): the bill carries no name, so neither is claimed.
  const projName = (id?: string) => (!id ? "—" : projects?.find((p) => p.id === id)?.name || `Project …${id.slice(-6)}`)

  const { data: bill, isLoading, error } = useQuery({
    queryKey: ["bill", bp, billId],
    queryFn: () => apiFetch<Bill>(`/bill/${bp}/${billId}`),
    enabled: !!bp && !!billId,
  })
  const { data: txns } = useQuery({
    queryKey: ["bill-transactions", bp, billId],
    queryFn: () => apiFetch<Transaction[]>(`/collect-transactions/${bp}/bill/${billId}`),
    enabled: !!bp && !!billId,
  })

  const invalidateBill = () => {
    void qc.invalidateQueries({ queryKey: ["bill", bp, billId] })
    void qc.invalidateQueries({ queryKey: ["bills", bp] })
    void qc.invalidateQueries({ queryKey: ["bill-transactions", bp, billId] })
    void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })
  }

  // Pay a SENT bill from the profile's credit balance (POST /payment/{bp}/bill/{billId}/pay,
  // no body; 400s: already-paid / open-bill / not-enough-credit).
  const payBill = useMutation({
    mutationFn: () => apiFetch(`/payment/${bp}/bill/${billId}/pay`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Bill paid from balance")
      setPayOpen(false)
      invalidateBill()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  // Charge a SENT bill to a saved card (POST /payment/{bp}/bill/{billId}/collect {cardId?}) — a
  // declined charge comes back as a FAILED transaction with errorMessage, not as an HTTP error.
  const { data: cards } = useQuery({
    queryKey: ["cards", bp],
    queryFn: () => apiFetch<CreditCard[]>(`/card/${bp}`),
    enabled: !!bp,
  })
  const [cardOpen, setCardOpen] = useState(false)
  const [cardChoice, setCardChoice] = useState("")
  const [cardError, setCardError] = useState<string | null>(null)
  const cardId = cardChoice || (cards?.some((c) => c.id === summary?.defaultCardId) ? summary?.defaultCardId ?? "" : "")
  const collect = useMutation({
    mutationFn: () =>
      apiFetch<CollectResult>(`/payment/${bp}/bill/${billId}/collect`, { method: "POST", body: cardId ? { cardId } : {} }),
    onSuccess: (txn) => {
      invalidateBill()
      if (txn?.status === "FAILED") {
        setCardError(txn.errorMessage || "The card was declined.")
        return
      }
      if (txn?.status === "SUCCESS") {
        toast.success(`Bill paid — charged ${fmtMoney(txn.grossAmount, txn.currency ?? bill?.invoiceCurrency)}`)
      } else {
        toast.info(`Payment ${String(txn?.status ?? "submitted").toLowerCase()}`)
      }
      setCardOpen(false)
    },
    onError: (e: Error) => setCardError(e.message),
  })
  const [newCardOpen, setNewCardOpen] = useState(false)

  // Pay a SENT bill by bank transfer: a deposit through the BankTransfer gateway with billId —
  // once an operator approves the transfer the minted credit settles this bill (server leg).
  const { data: gateways } = useQuery({
    queryKey: ["payment-gateways", bp],
    queryFn: () => apiFetch<Gateway[]>(`/payment/${bp}/gateway`),
    enabled: !!bp,
  })
  const bankGw = gateways?.find((g) => g.addFunds && g.thirdParty === "BankTransfer")
  // "Pay with new card": a card deposit with billId — the minted credit settles this bill.
  const newCardGws = cardGateways(gateways, "addFunds")
  const [bankOpen, setBankOpen] = useState(false)
  const [bankResult, setBankResult] = useState<DepositResponse | null>(null)
  const bankPay = useMutation({
    mutationFn: () =>
      apiFetch<DepositResponse>(`/payment/deposit/${bp}`, {
        method: "POST",
        body: { amount: Number(bill?.unpaidGrossAmount ?? 0), paymentGatewayId: bankGw?.id, billId },
      }),
    onSuccess: (resp) => {
      setBankResult(resp)
      void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })
      void qc.invalidateQueries({ queryKey: ["bank-transfers", bp] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const crumbs = (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbLink asChild>
            <Link to={`/p/${pid}/billing/history`}>Billing history</Link>
          </BreadcrumbLink>
        </BreadcrumbItem>
        <BreadcrumbSeparator />
        <BreadcrumbItem>
          <BreadcrumbPage className="font-mono text-xs">{billId}</BreadcrumbPage>
        </BreadcrumbItem>
      </BreadcrumbList>
    </Breadcrumb>
  )

  if (isLoading || !bp) {
    return (
      <>
        <PageHeader title="Bill" eyebrow="Billing" breadcrumb={crumbs} />
        <Skeleton className="h-72" />
      </>
    )
  }
  if (error || !bill) {
    return (
      <>
        <PageHeader title="Bill" eyebrow="Billing" breadcrumb={crumbs} />
        <div className="rounded-md border bg-muted/40 p-4 text-sm text-muted-foreground">
          {(error as Error | null)?.message ?? "Bill not found."}
        </div>
      </>
    )
  }

  const ccy = bill.invoiceCurrency
  const payable = bill.status === "SENT" && Number(bill.unpaidGrossAmount ?? 0) > 0
  const adjustments = (bill.adjustments as Array<Record<string, any>> | undefined) ?? []
  const promoCredits = (bill.appliedPromotionalCredits as Array<Record<string, any>> | undefined) ?? []
  const accountCredits = (bill.appliedAccountCredits as Array<Record<string, any>> | undefined) ?? []
  const appliedCredits: Array<Record<string, any> & { kind: string }> = [
    ...promoCredits.map((c) => ({ ...c, kind: "Promotional credit" })),
    ...accountCredits.map((c) => ({ ...c, kind: "Account credit" })),
  ]

  const allItems = bill.items ?? []
  const projectIds = [...new Set(allItems.map((it) => it.projectId as string).filter(Boolean))]
  const items = projFilter === "__all__" ? allItems : allItems.filter((it) => (it.projectId as string) === projFilter)
  const filtered = projFilter !== "__all__"
  const itemsSubtotal = items.reduce((s, it) => s + Number(it.netAmount ?? 0), 0)

  // Invoice lines that add up to the total: the bill's net already holds the adjustments, and the
  // gross is net plus tax rounded half-up to cents. Tax keeps its sign (a credit-note bill is
  // negative); the sub-cent part that rounding dropped gets its own line instead of reading as tax.
  // Credits reduce what was collected.
  const cents = (n: number) => (Math.sign(n) * Math.round(Math.abs(n) * 100)) / 100
  const net = Number(bill.netAmount ?? 0)
  const gross = Number(bill.grossAmount ?? 0)
  const adjTotal = adjustments.reduce((s, a) => s + Number(a.amount ?? 0), 0)
  const rounding = cents(net) - net
  const tax = cents(gross - cents(net))
  const unpaid = Number(bill.unpaidGrossAmount ?? 0)
  // An OPEN bill is still accruing: nothing is due on it yet, though the balance already counts it.
  const open = bill.status === "OPEN"

  return (
    <>
      <PageHeader
        title={`Bill — ${fmtDate(bill.createdAt)}`}
        eyebrow="Billing"
        breadcrumb={crumbs}
        description={`Created ${fmtDateTime(bill.createdAt)}`}
        actions={
          <>
            {payable ? (
              <Button size="sm" onClick={() => setPayOpen(true)}>
                <Wallet className="size-4" /> Pay with balance
              </Button>
            ) : null}
            {payable && cards?.length ? (
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  setCardError(null)
                  setCardOpen(true)
                }}
              >
                <CreditCardIcon className="size-4" /> Pay with saved card
              </Button>
            ) : null}
            {payable && newCardGws.length ? (
              <Button size="sm" variant="outline" onClick={() => setNewCardOpen(true)}>
                <CreditCardIcon className="size-4" /> Pay with new card
              </Button>
            ) : null}
            {payable && bankGw ? (
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  setBankResult(null)
                  setBankOpen(true)
                }}
              >
                <Landmark className="size-4" /> Pay by bank transfer
              </Button>
            ) : null}
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                downloadPdf(`/bill/${bp}/download/${billId}/statement`, `bill-${billId}-statement.pdf`).catch(
                  (e: Error) => toast.error(e.message),
                )
              }
            >
              <Download className="size-4" /> Download statement
            </Button>
          </>
        }
      />

      <Dialog open={bankOpen} onOpenChange={setBankOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Pay bill by bank transfer</DialogTitle>
            <DialogDescription>
              {bankResult
                ? "This bill is settled automatically once an operator confirms your transfer."
                : `Register a bank transfer for the unpaid ${fmtMoney(bill.unpaidGrossAmount, ccy)} on this bill. You will get a reference number and the bank details.`}
            </DialogDescription>
          </DialogHeader>
          {bankResult && bankGw ? (
            <BankTransferPanel transfer={transferFromDeposit(bankResult)} gateway={bankGw} bp={bp} currency={ccy} onCancelled={() => setBankResult(null)} />
          ) : null}
          <DialogFooter>
            {bankResult ? (
              <Button onClick={() => setBankOpen(false)}>Done</Button>
            ) : (
              <>
                <Button variant="outline" onClick={() => setBankOpen(false)}>
                  Cancel
                </Button>
                <Button onClick={() => bankPay.mutate()} disabled={bankPay.isPending}>
                  {bankPay.isPending ? "Registering…" : "Get transfer details"}
                </Button>
              </>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={cardOpen} onOpenChange={setCardOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Pay bill with a saved card</DialogTitle>
            <DialogDescription>
              Charge the unpaid {fmtMoney(bill.unpaidGrossAmount, ccy)} on this bill to one of your saved cards.
            </DialogDescription>
          </DialogHeader>
          <Select value={cardId} onValueChange={setCardChoice}>
            <SelectTrigger className="w-full font-mono" aria-label="Card">
              <SelectValue placeholder="Select a saved card" />
            </SelectTrigger>
            <SelectContent>
              {(cards ?? []).map((c) => (
                <SelectItem key={c.id} value={c.id} className="font-mono">
                  {c.panMasked ?? c.id}
                  {c.id === summary?.defaultCardId ? " · default" : ""}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {cardError ? <p className="text-sm text-destructive">{cardError}</p> : null}
          <DialogFooter>
            <Button variant="outline" onClick={() => setCardOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => collect.mutate()} disabled={!cardId || collect.isPending}>
              {collect.isPending ? "Charging…" : cardError ? "Retry" : "Pay bill"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {newCardGws.length ? (
        <NewCardDepositDialog
          open={newCardOpen}
          onOpenChange={setNewCardOpen}
          bp={bp}
          billId={billId}
          amount={unpaid}
          currency={ccy ?? summary?.currency ?? "USD"}
          gateways={newCardGws}
          preferredGatewayId={summary?.paymentGatewayId}
          onDone={invalidateBill}
        />
      ) : null}

      <Dialog open={payOpen} onOpenChange={setPayOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Pay bill with balance</DialogTitle>
            <DialogDescription>
              Pay the unpaid {fmtMoney(bill.unpaidGrossAmount, ccy)} on this bill from your credit balance?
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPayOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => payBill.mutate()} disabled={payBill.isPending}>
              {payBill.isPending ? "Paying…" : "Pay bill"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* A bank transfer registered for this bill and not yet confirmed: reference, instructions,
          and the customer's "I have transferred" step. */}
      {payable ? <PendingBankTransfers bp={bp} billId={billId} gateway={bankGw} currency={ccy} className="mb-4" /> : null}

      {/* The invoice: meta strip, line items, then the totals block — one printable surface. */}
      <Card className="gap-0 overflow-hidden py-0">
        <div className="grid grid-cols-2 gap-x-6 gap-y-4 border-b bg-muted/30 px-6 py-4 sm:grid-cols-4 lg:grid-cols-5">
          <Meta label={bill.invoiceNumber ? "Invoice no." : "Bill ID"}>
            <span className={bill.invoiceNumber ? "text-sm font-medium" : "font-mono text-sm"}>{bill.invoiceNumber ?? bill.id}</span>
          </Meta>
          <Meta label="Status">
            <StatusBadge status={bill.status} />
          </Meta>
          <Meta label="Issued">
            <span className="text-sm tabular-nums">{fmtDate(bill.createdAt)}</span>
          </Meta>
          <Meta label="Due">
            <span className="text-sm tabular-nums">{fmtDate(bill.dueAt)}</span>
          </Meta>
          {summary?.fullName ? (
            <Meta label="Billed to">
              <span className="text-sm">{summary.fullName}</span>
            </Meta>
          ) : null}
        </div>

        <div className="flex items-center justify-between gap-2 border-b px-6 py-3">
          <span className="text-eyebrow">Line items</span>
          {projectIds.length > 1 ? (
            <Select value={projFilter} onValueChange={setProjFilter}>
              <SelectTrigger size="sm" className="w-52">
                <SelectValue placeholder="All projects" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="__all__">All projects</SelectItem>
                {projectIds.map((id) => (
                  <SelectItem key={id} value={id}>
                    {projName(id)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : null}
        </div>

        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Item</TableHead>
              <TableHead>Project</TableHead>
              <TableHead>Resource type</TableHead>
              <TableHead className="text-right">Net amount</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.length ? (
              items.map((it, i) => (
                <TableRow key={i}>
                  <TableCell className="font-medium">{(it.name as string) ?? (it.resourceId as string) ?? "—"}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">{projName(it.projectId as string)}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">{(it.resourceType as string) ?? "—"}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">
                    {fmtMoney(it.netAmount as number, (it.currency as string) ?? ccy)}
                  </TableCell>
                </TableRow>
              ))
            ) : (
              <TableRow>
                <TableCell colSpan={4} className="py-6 text-center text-sm text-muted-foreground">
                  No items on this bill.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>

        {/* Totals — right-aligned invoice block. Bill-level: not affected by the project filter. */}
        <div className="flex justify-end border-t px-6 py-5">
          <div className="w-full max-w-sm space-y-1.5">
            {filtered ? (
              <>
                <TotalRow label={`Subtotal — ${projName(projFilter)}`} value={fmtMoney(itemsSubtotal, ccy)} muted />
                <div className="my-2 border-t border-dashed" />
              </>
            ) : null}
            <TotalRow label="Subtotal" value={fmtMoney(net - adjTotal, ccy)} />
            {adjustments.map((a, i) => (
              <TotalRow
                key={`adj-${i}`}
                label={(a.description as string) ?? (a.type as string) ?? "Adjustment"}
                value={fmtMoney(a.amount as number, ccy)}
                muted
              />
            ))}
            {Math.abs(rounding) >= 1e-6 ? <TotalRow label="Rounding" value={fmtMoney(rounding, ccy)} muted /> : null}
            <TotalRow label="Tax" value={fmtMoney(tax, ccy)} muted />
            <div className="my-2 border-t" />
            <TotalRow label="Total" value={fmtMoney(gross, ccy)} strong />
            {appliedCredits.length > 0 ? (
              <div className="pt-1.5">
                <div className="text-eyebrow mb-1.5">Credits applied</div>
                {appliedCredits.map((c, i) => (
                  <TotalRow
                    key={`credit-${i}`}
                    label={`${c.kind}${c.code ? ` · ${c.code as string}` : ""}`}
                    value={`−${fmtMoney(c.amount as number, ccy)}`}
                    accent
                  />
                ))}
              </div>
            ) : null}
            <div className="my-2 border-t" />
            {open ? (
              <div className="space-y-1">
                <div className="flex items-baseline justify-between gap-4">
                  <span className="text-sm font-medium">Accrued so far</span>
                  <span className="font-mono text-lg font-semibold tabular-nums">{fmtMoney(gross, ccy)}</span>
                </div>
                <p className="text-xs text-muted-foreground">
                  This bill is still open: charges add up until it is issued at the end of the billing cycle, and
                  nothing is due on it before then. Your balance already counts them.
                </p>
              </div>
            ) : (
              <div className="flex items-baseline justify-between gap-4">
                <span className="text-sm font-medium">Amount due</span>
                <span className="font-mono text-lg font-semibold tabular-nums">{fmtMoney(unpaid, ccy)}</span>
              </div>
            )}
          </div>
        </div>
      </Card>

      {/* Payments recorded against this bill. */}
      <Card className="mt-6 gap-0 overflow-hidden py-0">
        <div className="text-eyebrow border-b px-6 py-3">Payments</div>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Transaction</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Date</TableHead>
              <TableHead className="text-right">Amount</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {txns?.length ? (
              txns.map((t) => (
                <TableRow key={t.id}>
                  <TableCell className="font-mono text-xs">{(t.externalId as string) ?? t.id}</TableCell>
                  <TableCell>
                    <StatusBadge status={t.status} />
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{fmtDateTime(t.createdAt)}</TableCell>
                  <TableCell className="text-right font-mono tabular-nums">
                    {fmtMoney(t.grossAmount, t.currency)}
                  </TableCell>
                </TableRow>
              ))
            ) : (
              <TableRow>
                <TableCell colSpan={4} className="py-6 text-center text-sm text-muted-foreground">
                  No payments recorded for this bill.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </>
  )
}

function Meta({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="text-eyebrow mb-1">{label}</div>
      {children}
    </div>
  )
}

function TotalRow({
  label, value, muted, strong, accent,
}: {
  label: string
  value: string
  muted?: boolean
  strong?: boolean
  accent?: boolean
}) {
  return (
    <div className="flex items-baseline justify-between gap-4 text-sm">
      <span className={cn(muted || accent ? "text-muted-foreground" : "", strong && "font-medium")}>{label}</span>
      <span
        className={cn(
          "font-mono tabular-nums",
          muted && "text-muted-foreground",
          strong && "font-semibold",
          accent && "text-success-text",
        )}
      >
        {value}
      </span>
    </div>
  )
}
