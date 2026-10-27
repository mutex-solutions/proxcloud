import { useEffect, useRef, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { loadStripe, type Stripe, type StripeCardElement } from "@stripe/stripe-js"
import { BadgePercent, CreditCard as CreditCardIcon, Gift, Wallet } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatCard } from "@/components/stat-card"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Combobox } from "@/components/ui/combobox"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { fmtMoney, fmtMoneyTight } from "@/lib/format"
import { useBillingSummary, useProjectId } from "@/lib/hooks"
import type { CreditCard } from "@/lib/types"

type Gateway = {
  id?: string
  name?: string
  thirdParty?: string
  addFunds?: boolean
  minDeposit?: number | string
  metadata?: { publicKey?: string }
}

type Country = { name: string; cca2: string }

const TIERS = [10, 50, 100]

export default function FundsPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { data: summary, isLoading } = useBillingSummary(pid)
  const bp = summary?.id
  const currency = summary?.currency ?? "USD"
  // A NEW profile has no billing details yet — filling them activates it.
  const needsDetails = summary?.status === "NEW" || summary?.hasBillingDetails === false

  return (
    <>
      <PageHeader
        title="Funds"
        eyebrow="Billing"
        description="Balance, deposits and promo codes for this project's billing profile."
      />

      {isLoading || !bp ? (
        <Skeleton className="h-64" />
      ) : (
        <>
          <div className="grid gap-4 md:grid-cols-3">
            <StatCard
              label="Balance"
              icon={Wallet}
              numericValue={Number(summary?.balance ?? 0)}
              format={{ style: "currency", currency }}
              hint="Available on this billing profile"
            />
            <StatCard
              label="Account credit"
              icon={CreditCardIcon}
              numericValue={Number(summary?.accountCredit ?? 0)}
              format={{ style: "currency", currency }}
              hint="From deposits and refunds"
            />
            <StatCard
              label="Promotional credit"
              icon={Gift}
              numericValue={Number(summary?.promotionalCredit ?? 0)}
              format={{ style: "currency", currency }}
              hint="Applied to bills first"
            />
          </div>

          <div className="mt-6 grid items-start gap-4 lg:grid-cols-2">
            {needsDetails ? (
              <BillingDetailsCard bp={bp} onSaved={() => void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })} />
            ) : (
              <DepositCard pid={pid} bp={bp} currency={currency} defaultCardId={summary?.defaultCardId} />
            )}
            <PromoCard pid={pid} bp={bp} />
          </div>
        </>
      )}
    </>
  )
}

// Deposit via a saved card (POST /payment/deposit/{bp}/card, CollectRequest {cardId, amount})
// or via a new card (POST /payment/deposit/{bp} → PaymentIntent → Stripe Elements confirm).
function DepositCard({
  pid, bp, currency, defaultCardId,
}: { pid: string; bp: string; currency: string; defaultCardId?: string }) {
  const qc = useQueryClient()
  const [amount, setAmount] = useState("50")
  const [cardId, setCardId] = useState("")
  const [newCardOpen, setNewCardOpen] = useState(false)

  const { data: gateways } = useQuery({
    queryKey: ["payment-gateways", bp],
    queryFn: () => apiFetch<Gateway[]>(`/payment/${bp}/gateway`),
  })
  const { data: cards, isLoading: cardsLoading } = useQuery({
    queryKey: ["cards", bp],
    queryFn: () => apiFetch<CreditCard[]>(`/card/${bp}`),
  })

  // Preselect the profile's default card so the primary flow is one click —
  // never auto-pick a non-default card for a charge.
  useEffect(() => {
    if (!cardId && defaultCardId && cards?.some((c) => c.id === defaultCardId)) {
      setCardId(defaultCardId)
    }
  }, [cards, defaultCardId, cardId])

  const gw = gateways?.find((g) => g.addFunds)
  // New-card deposits confirm client-side with Stripe Elements — need the Stripe public key.
  const stripeGw = gateways?.find((g) => g.addFunds && g.thirdParty === "Stripe" && g.metadata?.publicKey)
  const minDeposit = Number(gw?.minDeposit ?? 0)

  const deposit = useMutation({
    mutationFn: () =>
      apiFetch<{ status?: string; grossAmount?: number }>(`/payment/deposit/${bp}/card`, {
        method: "POST",
        body: { cardId, amount: Number(amount) },
      }),
    onSuccess: (txn) => {
      if (txn?.status === "SUCCESS") {
        toast.success(`Deposit succeeded — charged ${fmtMoney(txn.grossAmount, currency)} (incl. tax)`)
      } else {
        toast.info(`Deposit ${String(txn?.status ?? "submitted").toLowerCase()}`)
      }
      void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

  const amountNum = Number(amount)
  const amountInvalid = !Number.isFinite(amountNum) || amountNum <= 0 || (minDeposit > 0 && amountNum < minDeposit)

  return (
    <Card className="gap-4">
      <CardHeader className="border-b [.border-b]:pb-4">
        <CardTitle className="text-base">Add funds</CardTitle>
        <CardDescription>Deposits become account credit and settle future bills.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        <div>
          <Label className="mb-2 block" htmlFor="deposit-amount">Amount</Label>
          <div className="relative">
            <Input
              id="deposit-amount"
              className="h-12 pr-16 font-mono text-2xl font-medium tabular-nums md:text-2xl"
              type="number"
              inputMode="decimal"
              min={minDeposit || 1}
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
            <span className="pointer-events-none absolute top-1/2 right-3 -translate-y-1/2 font-mono text-xs text-muted-foreground">
              {currency}
            </span>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-2">
            {TIERS.map((t) => (
              <Button
                key={t}
                type="button"
                variant={amount === String(t) ? "secondary" : "outline"}
                size="sm"
                className="font-mono tabular-nums"
                onClick={() => setAmount(String(t))}
              >
                {fmtMoneyTight(t, currency)}
              </Button>
            ))}
            {minDeposit > 0 ? (
              <span className="text-xs text-muted-foreground">
                Minimum {fmtMoneyTight(minDeposit, currency)}
              </span>
            ) : null}
          </div>
        </div>

        <div>
          <Label className="mb-2 block">Pay with card</Label>
          {cardsLoading ? (
            <Skeleton className="h-9" />
          ) : cards?.length ? (
            <Select value={cardId} onValueChange={setCardId}>
              <SelectTrigger className="w-full font-mono" aria-label="Pay with card">
                <SelectValue placeholder="Select a saved card" />
              </SelectTrigger>
              <SelectContent>
                {cards.map((c) => (
                  <SelectItem key={c.id} value={c.id} className="font-mono">
                    {c.panMasked ?? c.id}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <p className="text-sm text-muted-foreground">
              No saved cards.{" "}
              <Link className="underline" to={`/p/${pid}/billing/cards`}>
                Add a card
              </Link>{" "}
              first.
            </p>
          )}
        </div>

        <div className="flex flex-wrap gap-2">
          <Button
            onClick={() => deposit.mutate()}
            disabled={!gw || !cardId || amountInvalid || deposit.isPending}
          >
            {deposit.isPending ? "Depositing…" : "Deposit"}
          </Button>
          <Button
            variant="outline"
            onClick={() => setNewCardOpen(true)}
            disabled={!stripeGw || amountInvalid}
          >
            Deposit with new card
          </Button>
        </div>
        {!gw && gateways ? (
          <p className="text-xs text-muted-foreground">No payment gateway is configured for deposits.</p>
        ) : null}

        {stripeGw ? (
          <NewCardDepositDialog
            open={newCardOpen}
            onOpenChange={setNewCardOpen}
            bp={bp}
            amount={amountNum}
            currency={currency}
            gatewayId={stripeGw.id ?? ""}
            publicKey={stripeGw.metadata?.publicKey ?? ""}
            onDone={() => void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })}
          />
        ) : null}
      </CardContent>
    </Card>
  )
}

type DepositResponse = { transactionId?: string; externalPaymentId?: string; metadata?: unknown }

// Card-less deposit: POST /payment/deposit/{bp} {amount, paymentGatewayId} → the response
// `metadata` IS the PaymentIntent client secret (a string, per the Go AddFunds handler) →
// Stripe Elements confirmCardPayment → GET the whitelisted funds-confirm callback (302) so the
// API retrieves the PaymentIntent and mints the account credit.
function NewCardDepositDialog({
  open, onOpenChange, bp, amount, currency, gatewayId, publicKey, onDone,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  bp: string
  amount: number
  currency: string
  gatewayId: string
  publicKey: string
  onDone: () => void
}) {
  const mountRef = useRef<HTMLDivElement>(null)
  const stripeRef = useRef<Stripe | null>(null)
  const cardRef = useRef<StripeCardElement | null>(null)
  const [intent, setIntent] = useState<{ txnId: string; clientSecret: string } | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [paying, setPaying] = useState(false)

  useEffect(() => {
    if (!open) {
      setIntent(null)
      setError(null)
      return
    }
    let cancelled = false
    ;(async () => {
      try {
        const resp = await apiFetch<DepositResponse>(`/payment/deposit/${bp}`, {
          method: "POST",
          body: { amount, paymentGatewayId: gatewayId },
        })
        const clientSecret = typeof resp?.metadata === "string" ? resp.metadata : undefined
        if (!resp?.transactionId || !clientSecret) throw new Error("Gateway did not return a payment secret")
        const stripe = await loadStripe(publicKey)
        if (cancelled) return
        if (!stripe) throw new Error("Stripe failed to load")
        stripeRef.current = stripe
        const card = stripe.elements().create("card")
        cardRef.current = card
        if (mountRef.current) card.mount(mountRef.current)
        setIntent({ txnId: resp.transactionId, clientSecret })
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      cancelled = true
      cardRef.current?.unmount()
      cardRef.current = null
    }
  }, [open, bp, gatewayId, publicKey, amount])

  const pay = async () => {
    const stripe = stripeRef.current
    const card = cardRef.current
    if (!stripe || !card || !intent) return
    setPaying(true)
    setError(null)
    try {
      const res = await stripe.confirmCardPayment(intent.clientSecret, { payment_method: { card } })
      if (res.error) throw new Error(res.error.message ?? "Payment confirmation failed")
      // Finalize server-side (whitelisted callback: retrieves the PaymentIntent + mints the
      // account credit, returns 200; throws here if the server couldn't finalize).
      await apiFetch(`/callbacks/payment/stripe/funds/confirm/${intent.txnId}`)
      toast.success(`Deposit of ${fmtMoney(amount, currency)} succeeded`)
      onDone()
      onOpenChange(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setPaying(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            Deposit <span className="font-mono tabular-nums">{fmtMoney(amount, currency)}</span> with a new card
          </DialogTitle>
          <DialogDescription>Card details go directly to Stripe — they never touch ProxCloud.</DialogDescription>
        </DialogHeader>
        <div className="rounded-md border bg-muted/30 p-3">
          <div ref={mountRef} />
        </div>
        {error ? <p className="text-sm text-destructive">{error}</p> : null}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={() => void pay()} disabled={!intent || paying}>
            {paying ? "Paying…" : "Pay"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// Redeem a promo code (POST /promotion/{bp}/code?code=X → mints a promotional credit).
function PromoCard({ pid, bp }: { pid: string; bp: string }) {
  const qc = useQueryClient()
  const [code, setCode] = useState("")
  const redeem = useMutation({
    mutationFn: () =>
      apiFetch(`/promotion/${bp}/code?code=${encodeURIComponent(code.trim())}`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Promo code redeemed")
      setCode("")
      void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  return (
    <Card className="gap-4">
      <CardHeader className="border-b [.border-b]:pb-4">
        <CardTitle className="flex items-center gap-2 text-base">
          <BadgePercent className="size-4 text-primary" /> Redeem a promo code
        </CardTitle>
        <CardDescription>Promo codes mint promotional credit onto this profile.</CardDescription>
      </CardHeader>
      <CardContent className="flex gap-2">
        <Input
          placeholder="Promo code"
          aria-label="Promo code"
          autoComplete="off"
          className="font-mono"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && code.trim() && !redeem.isPending) redeem.mutate()
          }}
        />
        <Button onClick={() => redeem.mutate()} disabled={!code.trim() || redeem.isPending}>
          {redeem.isPending ? "Redeeming…" : "Redeem"}
        </Button>
      </CardContent>
    </Card>
  )
}

// First-time billing details (PUT /billing-profile/{bp}) — saving valid details activates a NEW profile.
function BillingDetailsCard({ bp, onSaved }: { bp: string; onSaved: () => void }) {
  const [form, setForm] = useState({
    firstName: "", lastName: "", phone: "", address: "", city: "", zipCode: "", country: "",
  })
  const { data: countries } = useQuery({
    queryKey: ["countries"],
    queryFn: () => apiFetch<Country[]>("/billing-profile/countries"),
  })
  const save = useMutation({
    mutationFn: () =>
      apiFetch(`/billing-profile/${bp}`, { method: "PUT", body: { ...form, company: false } }),
    onSuccess: () => {
      toast.success("Billing details saved — profile activated")
      onSaved()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }))
  const incomplete = Object.values(form).some((v) => !v.trim())

  return (
    <Card className="gap-4">
      <CardHeader className="border-b [.border-b]:pb-4">
        <CardTitle className="text-base">Billing details</CardTitle>
        <CardDescription>
          Fill in your billing address to activate this billing profile and unlock deposits.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <Field label="First name" htmlFor="bd-first-name">
            <Input id="bd-first-name" name="given-name" autoComplete="given-name" value={form.firstName} onChange={set("firstName")} />
          </Field>
          <Field label="Last name" htmlFor="bd-last-name">
            <Input id="bd-last-name" name="family-name" autoComplete="family-name" value={form.lastName} onChange={set("lastName")} />
          </Field>
          <Field label="Street address" htmlFor="bd-address" className="sm:col-span-2">
            <Input id="bd-address" name="street-address" autoComplete="street-address" value={form.address} onChange={set("address")} />
          </Field>
          <Field label="City" htmlFor="bd-city">
            <Input id="bd-city" name="city" autoComplete="address-level2" value={form.city} onChange={set("city")} />
          </Field>
          <Field label="ZIP code" htmlFor="bd-zip">
            <Input id="bd-zip" name="postal-code" autoComplete="postal-code" value={form.zipCode} onChange={set("zipCode")} />
          </Field>
          <Field label="Country">
            <Combobox
              options={(countries ?? []).map((c) => ({ value: c.cca2, label: c.name }))}
              value={form.country}
              onValueChange={(v) => setForm((f) => ({ ...f, country: v }))}
              placeholder="Select country"
              searchPlaceholder="Search country…"
            />
          </Field>
          <Field label="Phone" htmlFor="bd-phone">
            <Input id="bd-phone" name="tel" type="tel" autoComplete="tel" placeholder="+1 555 0100" value={form.phone} onChange={set("phone")} />
          </Field>
        </div>
        <Button onClick={() => save.mutate()} disabled={incomplete || save.isPending}>
          {save.isPending ? "Saving…" : "Save billing details"}
        </Button>
      </CardContent>
    </Card>
  )
}

function Field({
  label, htmlFor, className, children,
}: { label: string; htmlFor?: string; className?: string; children: React.ReactNode }) {
  return (
    <div className={className}>
      <Label className="mb-1.5 block" htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  )
}
