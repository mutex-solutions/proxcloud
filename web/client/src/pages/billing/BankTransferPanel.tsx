import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { CheckCircle2, Landmark } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { apiFetch } from "@/lib/api"
import { fmtDate, fmtDateTime, fmtMoney } from "@/lib/format"
import type { Gateway } from "@/lib/types"

// POST /payment/deposit/{bp} response. For a BankTransfer gateway `metadata` is
// {referenceNumber} and externalPaymentId is the bankTransfer id; for Stripe `metadata` is the
// PaymentIntent client secret (a string).
export type DepositResponse = {
  transactionId?: string
  externalPaymentId?: string
  thirdParty?: string
  grossAmount?: number | string
  invoiceCurrency?: string
  metadata?: unknown
}

// A bank transfer as listed by GET /payment/{bp}/bank-transfers (proof reduced to its metadata).
export type BankTransfer = {
  id: string
  status?: string
  referenceNumber?: number | string
  grossAmount?: number | string
  currency?: string
  createdAt?: string
  billId?: string
  invoiceNumber?: string
  submittedAt?: string
  transferredAt?: string
  bankReference?: string
  note?: string
  proof?: { fileName?: string; contentType?: string; size?: number }
}

// The transfer a fresh deposit response describes (the list will carry the same doc later).
export function transferFromDeposit(resp: DepositResponse): BankTransfer {
  const md = resp.metadata
  const ref = md && typeof md === "object" ? (md as { referenceNumber?: unknown }).referenceNumber : undefined
  return {
    id: resp.externalPaymentId ?? "",
    status: "PENDING",
    referenceNumber: ref === undefined || ref === null ? undefined : String(ref),
    grossAmount: resp.grossAmount,
    currency: resp.invoiceCurrency,
  }
}

export const PROOF_MAX_BYTES = 2 * 1024 * 1024
const PROOF_TYPES = ["application/pdf", "image/png", "image/jpeg"]

function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onerror = () => reject(new Error("Could not read the file"))
    r.onload = () => resolve(String(r.result).split(",")[1] ?? "")
    r.readAsDataURL(file)
  })
}

// Transfer card: what to transfer, where, the reference to quote — and, while the transfer is
// PENDING, the "I have transferred" form (POST .../bank-transfers/{id}/submit). Once submitted it
// shows the submission and waits for the operator's confirmation.
export function BankTransferPanel({
  transfer, gateway, bp, currency, onCancelled,
}: { transfer: BankTransfer; gateway?: Gateway; bp: string; currency?: string; onCancelled?: () => void }) {
  const qc = useQueryClient()
  const [cancelled, setCancelled] = useState(false)
  const instructions = gateway?.metadata?.instructions?.trim()
  const [date, setDate] = useState(() => new Date().toISOString().slice(0, 10))
  const [bankRef, setBankRef] = useState("")
  const [note, setNote] = useState("")
  const [file, setFile] = useState<File | null>(null)
  const [submitted, setSubmitted] = useState<BankTransfer | null>(null)
  const shown = submitted ?? transfer

  const submit = useMutation({
    mutationFn: async () => {
      const body: Record<string, unknown> = { bankReference: bankRef.trim(), note: note.trim() }
      if (date) body.transferredAt = new Date(`${date}T00:00:00Z`).toISOString()
      if (file) {
        if (!PROOF_TYPES.includes(file.type)) throw new Error("Proof must be a PDF, PNG or JPEG file")
        if (file.size > PROOF_MAX_BYTES) throw new Error("Proof file must be at most 2 MB")
        body.proof = { fileName: file.name, contentType: file.type, dataBase64: await fileToBase64(file) }
      }
      return apiFetch<BankTransfer>(`/payment/${bp}/bank-transfers/${transfer.id}/submit`, { method: "POST", body })
    },
    onSuccess: (t) => {
      setSubmitted(t)
      toast.success("Thanks — we will confirm your transfer shortly")
      void qc.invalidateQueries({ queryKey: ["bank-transfers", bp] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  // Withdraw a transfer the customer never made (POST .../cancel). Only offered before "I have
  // transferred": once reported, money may be in flight and only the operator can resolve it.
  const cancel = useMutation({
    mutationFn: () => apiFetch<BankTransfer>(`/payment/${bp}/bank-transfers/${transfer.id}/cancel`, { method: "POST" }),
    onSuccess: () => {
      setCancelled(true)
      toast.success("Bank transfer cancelled")
      void qc.invalidateQueries({ queryKey: ["bank-transfers", bp] })
      onCancelled?.()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const pending = shown.status === "PENDING"
  if (cancelled) return null
  return (
    <div className="space-y-3 rounded-md border bg-muted/30 p-4 text-sm">
      <div className="flex items-center justify-between gap-2 font-medium">
        <span className="flex items-center gap-2">
          <Landmark className="size-4 text-primary" /> Bank transfer{shown.invoiceNumber ? ` · ${shown.invoiceNumber}` : ""}
        </span>
        {shown.createdAt ? <span className="text-xs font-normal text-muted-foreground">{fmtDate(shown.createdAt)}</span> : null}
      </div>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5">
        <dt className="text-muted-foreground">Amount to transfer</dt>
        <dd className="font-mono tabular-nums">{fmtMoney(shown.grossAmount, shown.currency ?? currency)}</dd>
        <dt className="text-muted-foreground">Reference number</dt>
        <dd className="font-mono">{shown.referenceNumber == null ? "—" : String(shown.referenceNumber)}</dd>
      </dl>
      {instructions ? (
        <div>
          <div className="text-eyebrow mb-1">Bank details</div>
          <pre className="whitespace-pre-wrap font-sans">{instructions}</pre>
        </div>
      ) : null}

      {shown.submittedAt ? (
        <div className="flex items-start gap-2 rounded-md border bg-background p-3">
          <CheckCircle2 className="mt-0.5 size-4 text-primary" />
          <div>
            <div className="font-medium">Submitted — awaiting confirmation</div>
            <div className="text-muted-foreground">
              Reported on {fmtDateTime(shown.submittedAt)}
              {shown.transferredAt ? ` · transferred ${fmtDate(shown.transferredAt)}` : ""}
              {shown.bankReference ? ` · ref ${shown.bankReference}` : ""}
              {shown.proof?.fileName ? ` · ${shown.proof.fileName}` : ""}
            </div>
          </div>
        </div>
      ) : pending && transfer.id ? (
        <form
          className="space-y-3 rounded-md border bg-background p-3"
          onSubmit={(e) => {
            e.preventDefault()
            submit.mutate()
          }}
        >
          <div className="font-medium">I have transferred</div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor={`bt-date-${transfer.id}`}>Transfer date</Label>
              <Input id={`bt-date-${transfer.id}`} type="date" value={date} onChange={(e) => setDate(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor={`bt-ref-${transfer.id}`}>Bank reference</Label>
              <Input
                id={`bt-ref-${transfer.id}`}
                maxLength={120}
                placeholder="Your bank's transaction reference"
                value={bankRef}
                onChange={(e) => setBankRef(e.target.value)}
              />
            </div>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor={`bt-note-${transfer.id}`}>Note</Label>
            <Textarea id={`bt-note-${transfer.id}`} maxLength={500} rows={2} value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor={`bt-proof-${transfer.id}`}>Proof of payment (PDF, PNG or JPEG, max 2 MB)</Label>
            <Input
              id={`bt-proof-${transfer.id}`}
              type="file"
              accept={PROOF_TYPES.join(",")}
              onChange={(e) => setFile(e.target.files?.[0] ?? null)}
            />
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" size="sm" disabled={submit.isPending || cancel.isPending}>
              {submit.isPending ? "Submitting…" : "I have transferred"}
            </Button>
            <Button
              type="button"
              size="sm"
              variant="ghost"
              disabled={submit.isPending || cancel.isPending}
              onClick={() => cancel.mutate()}
            >
              {cancel.isPending ? "Cancelling…" : "Cancel this transfer"}
            </Button>
          </div>
        </form>
      ) : null}

      <p className="text-muted-foreground">
        Quote the reference number in your transfer. We credit your account once the transfer is
        confirmed — these details were also sent to your billing email.
      </p>
    </div>
  )
}

// The profile's bank transfers (GET /payment/{bp}/bank-transfers), newest first.
export function useBankTransfers(bp: string | undefined) {
  return useQuery({
    queryKey: ["bank-transfers", bp],
    queryFn: () => apiFetch<BankTransfer[]>(`/payment/${bp}/bank-transfers`),
    enabled: !!bp,
  })
}

// Every still-PENDING transfer of the profile (optionally only those paying `billId`), each with
// its submit form — so a customer who closed the deposit dialog can still report the transfer.
export function PendingBankTransfers({
  bp, billId, gateway, currency, exclude, className,
}: { bp: string; billId?: string; gateway?: Gateway; currency?: string; exclude?: string; className?: string }) {
  const { data } = useBankTransfers(bp)
  const pending = (data ?? []).filter(
    (t) => t.status === "PENDING" && t.id !== exclude && (!billId || t.billId === billId),
  )
  if (!pending.length) return null
  return (
    <div className={className ? `space-y-3 ${className}` : "space-y-3"}>
      {pending.map((t) => (
        <BankTransferPanel key={t.id} transfer={t} gateway={gateway} bp={bp} currency={currency} />
      ))}
    </div>
  )
}
