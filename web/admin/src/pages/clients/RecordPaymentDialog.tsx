import { useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
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
import { useAdminGet } from "@/lib/hooks"
import { fmtMoney } from "@/lib/format"

// POST /admin/bill/{id}/record-payment {amount, method, reference, paidAt} (admin/bill.go
// billRecordPayment, permission admin:bill:manage). The accountant confirms an offline payment on a
// SENT bill: amount = the GROSS figure received in the invoice currency (default: the full unpaid
// amount → the bill flips PAID; a partial amount leaves it SENT with the collected amount recorded).
export type RecordPaymentBill = {
  id: string
  /** Unpaid gross amount when the caller already has the overview; omitted → fetched from GET /admin/bill/{id}. */
  unpaidAmount?: number | string
  currency?: string
}

type Method = "BANK_TRANSFER" | "OTHER"

// "YYYY-MM-DDTHH:mm" in local time for <input type="datetime-local">.
function localNow(): string {
  const d = new Date()
  d.setMinutes(d.getMinutes() - d.getTimezoneOffset())
  return d.toISOString().slice(0, 16)
}

export function RecordPaymentDialog({ bill, onClose }: { bill: RecordPaymentBill | null; onClose: () => void }) {
  return (
    <Dialog open={!!bill} onOpenChange={(o) => !o && onClose()}>
      {bill ? <RecordPaymentForm key={bill.id} bill={bill} onClose={onClose} /> : null}
    </Dialog>
  )
}

// Keyed by bill id from the parent so every open starts from a fresh, prefilled form.
function RecordPaymentForm({ bill, onClose }: { bill: RecordPaymentBill; onClose: () => void }) {
  const qc = useQueryClient()
  // List rows (GET /admin/bill) carry no unpaid amount — fetch the overview when it wasn't passed in.
  const overview = useAdminGet<Record<string, any>>(`/admin/bill/${bill.id}`, bill.unpaidAmount === undefined)
  const unpaid = bill.unpaidAmount ?? overview.data?.unpaidAmount
  const currency = bill.currency ?? overview.data?.invoiceCurrency ?? overview.data?.currency

  const [amount, setAmount] = useState<string | null>(null) // null = untouched → prefilled with unpaid
  const [method, setMethod] = useState<Method>("BANK_TRANSFER")
  const [reference, setReference] = useState("")
  const [paidAt, setPaidAt] = useState(localNow)
  const shownAmount = amount ?? (unpaid !== undefined ? String(unpaid) : "")
  const amt = parseFloat(shownAmount)

  const record = useMutation({
    mutationFn: () =>
      apiFetch<Record<string, any>>(`/admin/bill/${bill.id}/record-payment`, {
        method: "POST",
        body: { amount: shownAmount, method, reference, paidAt: new Date(paidAt).toISOString() },
      }),
    onSuccess: (res) => {
      toast.success(res?.status === "PAID" ? "Payment recorded — bill is paid" : "Partial payment recorded")
      void qc.invalidateQueries({ queryKey: ["admin-list"] })
      void qc.invalidateQueries({ queryKey: ["admin-get"] })
      void qc.invalidateQueries({ queryKey: ["admin-bp"] })
      onClose()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <DialogContent>
      <DialogHeader>
        <DialogTitle>Record payment</DialogTitle>
        <DialogDescription>
          Confirm an offline payment received for bill <span className="font-mono text-xs">{bill.id}</span>.
          {unpaid !== undefined ? <> Unpaid: {fmtMoney(unpaid, currency)}.</> : null} Paying the full amount closes
          the bill; a smaller amount records a partial payment.
        </DialogDescription>
      </DialogHeader>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          record.mutate()
        }}
      >
        <div className="grid gap-2">
          <Label htmlFor="rp-amount">Amount received ({currency ?? "invoice currency"})</Label>
          <Input
            id="rp-amount"
            type="number"
            min="0"
            step="0.01"
            required
            value={shownAmount}
            placeholder={overview.isLoading ? "Loading…" : "0.00"}
            onChange={(e) => setAmount(e.target.value)}
          />
        </div>
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="rp-method">Method</Label>
            <Select value={method} onValueChange={(v) => setMethod(v as Method)}>
              <SelectTrigger id="rp-method" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="BANK_TRANSFER">Bank transfer</SelectItem>
                <SelectItem value="OTHER">Other</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="rp-date">Paid on</Label>
            <Input id="rp-date" type="datetime-local" required value={paidAt} onChange={(e) => setPaidAt(e.target.value)} />
          </div>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="rp-ref">Reference</Label>
          <Input
            id="rp-ref"
            autoComplete="off"
            placeholder="Bank statement reference, receipt number…"
            value={reference}
            onChange={(e) => setReference(e.target.value)}
          />
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={!(amt > 0) || !paidAt || record.isPending}>
            {record.isPending ? "Recording…" : "Record payment"}
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  )
}
