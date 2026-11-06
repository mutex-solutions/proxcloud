import { useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { loadStripe } from "@stripe/stripe-js"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { loadAirwallexCard, type AirwallexCard } from "@/lib/airwallex"
import { apiFetch } from "@/lib/api"
import { fmtMoney } from "@/lib/format"
import type { Gateway } from "@/lib/types"
import type { DepositResponse } from "./BankTransferPanel"

// Gateways whose card element can be mounted in the browser: Stripe (needs the Elements public
// key) and Airwallex. `use` is the capability needed: adding a card or depositing with a new one.
export function cardGateways(gateways: Gateway[] | undefined, use: "addCard" | "addFunds"): Gateway[] {
  return (gateways ?? []).filter(
    (g) => g[use] && (g.thirdParty === "Airwallex" || (g.thirdParty === "Stripe" && !!g.metadata?.publicKey)),
  )
}

// The profile's preferred gateway when it is one of `gateways`, else the first one.
export function defaultGatewayId(gateways: Gateway[], preferredId?: string): string {
  return (gateways.find((g) => g.id === preferredId) ?? gateways[0])?.id ?? ""
}

export const gatewayLabel = (g: Gateway) => (g.name ? `${g.name} · ${g.thirdParty}` : g.thirdParty ?? g.id ?? "")
// The {stripe|airwallex} segment of the confirm callbacks.
export const gatewaySlug = (g: Gateway | undefined) => (g?.thirdParty ?? "").toLowerCase()
// Airwallex rejects with a plain {code, message} object, not an Error.
export const errMsg = (e: unknown) =>
  e instanceof Error ? e.message : ((e as { message?: string } | null)?.message ?? String(e))

export function GatewaySelect({
  gateways, value, onChange, disabled,
}: { gateways: Gateway[]; value: string; onChange: (id: string) => void; disabled?: boolean }) {
  if (gateways.length < 2) return null
  return (
    <div>
      <Label className="mb-2 block">Payment gateway</Label>
      <Select value={value} onValueChange={onChange} disabled={disabled}>
        <SelectTrigger className="w-full" aria-label="Payment gateway">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {gateways.map((g) => (
            <SelectItem key={g.id} value={g.id ?? ""}>
              {gatewayLabel(g)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

type AirwallexConfirm = Parameters<AirwallexCard["confirm"]>[0]
type ConfirmArgs = { clientSecret: string; intentId: string; airwallex?: Partial<AirwallexConfirm> }

// Mounts the gateway's hosted card input into `mountRef` while `active`, and confirms a
// PaymentIntent with it: Stripe → confirmCardPayment(client_secret), Airwallex → card.confirm
// ({intent_id, client_secret, ...airwallex}). Card data never touches ProxCloud either way.
export function useCardElement(gateway: Gateway | undefined, active: boolean) {
  const mountRef = useRef<HTMLDivElement>(null)
  const confirmRef = useRef<((a: ConfirmArgs) => Promise<void>) | null>(null)
  const [ready, setReady] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const kind = gateway?.thirdParty
  const publicKey = gateway?.metadata?.publicKey
  const env = gateway?.metadata?.env

  useEffect(() => {
    setReady(false)
    setError(null)
    confirmRef.current = null
    if (!active || !kind) return
    let cancelled = false
    let cleanup = () => {}
    // The dialog content mounts (portal + presence) a tick after `active` flips, so the container
    // may not exist yet when this effect runs: wait for it instead of silently bailing out.
    const container = async () => {
      for (let i = 0; i < 40 && !mountRef.current && !cancelled; i++) await new Promise((r) => setTimeout(r, 50))
      return mountRef.current
    }
    ;(async () => {
      try {
        if (kind === "Stripe") {
          const stripe = await loadStripe(publicKey ?? "")
          const el = await container()
          if (cancelled || !el) return
          if (!stripe) throw new Error("Stripe failed to load")
          const card = stripe.elements().create("card")
          card.mount(el)
          cleanup = () => card.unmount()
          confirmRef.current = async ({ clientSecret }) => {
            const res = await stripe.confirmCardPayment(clientSecret, { payment_method: { card } })
            if (res.error) throw new Error(res.error.message ?? "Card confirmation failed")
          }
        } else if (kind === "Airwallex") {
          const el = await container()
          if (cancelled || !el) return
          const { card, unmount } = await loadAirwallexCard(env === "prod" ? "prod" : "demo", el)
          if (cancelled) {
            unmount()
            return
          }
          cleanup = unmount
          confirmRef.current = async ({ clientSecret, intentId, airwallex }) => {
            await card.confirm({ ...airwallex, intent_id: intentId, client_secret: clientSecret })
          }
        } else {
          throw new Error(`Unsupported card gateway: ${kind}`)
        }
        setReady(true)
      } catch (e) {
        if (!cancelled) setError(errMsg(e))
      }
    })()
    return () => {
      cancelled = true
      cleanup()
      confirmRef.current = null
    }
  }, [active, kind, publicKey, env])

  const confirm = async (a: ConfirmArgs) => {
    if (!confirmRef.current) throw new Error("The card form is not ready yet")
    await confirmRef.current(a)
  }
  return { mountRef, ready, error, confirm }
}

// Card-less deposit: POST /payment/deposit/{bp} {amount, paymentGatewayId, billId?} → the
// response `metadata` IS the PaymentIntent client secret (a string) → confirm it with the
// gateway's card element → GET the whitelisted funds-confirm callback so the API retrieves the
// intent and mints the account credit (which settles `billId` when given).
export function NewCardDepositDialog({
  open, onOpenChange, bp, amount, currency, gateways, preferredGatewayId, billId, onDone,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  bp: string
  amount: number
  currency: string
  gateways: Gateway[]
  preferredGatewayId?: string
  billId?: string
  onDone: () => void
}) {
  const [gwId, setGwId] = useState(() => defaultGatewayId(gateways, preferredGatewayId))
  const gateway = gateways.find((g) => g.id === gwId)
  const el = useCardElement(gateway, open)
  const [intent, setIntent] = useState<{ txnId: string; intentId: string; clientSecret: string } | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [paying, setPaying] = useState(false)

  useEffect(() => {
    setIntent(null)
    setError(null)
    if (!open) {
      setGwId(defaultGatewayId(gateways, preferredGatewayId))
      return
    }
    if (!gwId) return
    let cancelled = false
    ;(async () => {
      try {
        const resp = await apiFetch<DepositResponse>(`/payment/deposit/${bp}`, {
          method: "POST",
          body: { amount, paymentGatewayId: gwId, ...(billId ? { billId } : {}) },
        })
        const clientSecret = typeof resp?.metadata === "string" ? resp.metadata : undefined
        if (!resp?.transactionId || !clientSecret) throw new Error("Gateway did not return a payment secret")
        if (!cancelled) setIntent({ txnId: resp.transactionId, intentId: resp.externalPaymentId ?? "", clientSecret })
      } catch (e) {
        if (!cancelled) setError(errMsg(e))
      }
    })()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, bp, gwId, amount, billId])

  const pay = async () => {
    if (!intent) return
    setPaying(true)
    setError(null)
    try {
      await el.confirm({ clientSecret: intent.clientSecret, intentId: intent.intentId })
      await apiFetch(`/callbacks/payment/${gatewaySlug(gateway)}/funds/confirm/${intent.txnId}`)
      toast.success(billId ? "Bill paid" : `Deposit of ${fmtMoney(amount, currency)} succeeded`)
      onDone()
      onOpenChange(false)
    } catch (e) {
      setError(errMsg(e))
    } finally {
      setPaying(false)
    }
  }

  const shownError = error ?? el.error
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {billId ? "Pay" : "Deposit"} <span className="font-mono tabular-nums">{fmtMoney(amount, currency)}</span> with a new card
          </DialogTitle>
          <DialogDescription>
            Card details go directly to {gateway?.thirdParty ?? "the payment gateway"} — they never touch ProxCloud.
          </DialogDescription>
        </DialogHeader>
        <GatewaySelect gateways={gateways} value={gwId} onChange={setGwId} disabled={paying} />
        <div className="rounded-md border bg-muted/30 p-3">
          <div ref={el.mountRef} />
        </div>
        {shownError ? <p className="text-sm text-destructive">{shownError}</p> : null}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={() => void pay()} disabled={!intent || !el.ready || paying}>
            {paying ? "Paying…" : "Pay"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
