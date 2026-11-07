import { useEffect, useState } from "react"
import { Link } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { BadgePercent, CreditCard as CreditCardIcon, Gift, Wallet } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { StatCard } from "@/components/stat-card"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
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
import type { CreditCard, Gateway } from "@/lib/types"
import {
  BankTransferPanel, PendingBankTransfers, transferFromDeposit, type DepositResponse,
} from "./BankTransferPanel"
import { NewCardDepositDialog, cardGateways } from "./NewCardDepositDialog"

type Country = { name: string; cca2: string }

const TIERS = [10, 50, 100]

export default function FundsPage() {
  const pid = useProjectId()
  const qc = useQueryClient()
  const { data: summary, isLoading } = useBillingSummary(pid)
  const bp = summary?.id
  const currency = summary?.currency ?? "USD"
  // A NEW profile has no billing details yet — filling them activates it. An operator can also
  // activate a profile with none: deposits then work, and the address is only asked for.
  const needsDetails = summary?.status === "NEW"
  const missingDetails = !needsDetails && summary?.hasBillingDetails === false

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
              <DepositCard
                pid={pid}
                bp={bp}
                currency={currency}
                defaultCardId={summary?.defaultCardId}
                preferredGatewayId={summary?.paymentGatewayId}
              />
            )}
            <PromoCard pid={pid} bp={bp} />
            {missingDetails ? (
              <BillingDetailsCard
                bp={bp}
                activated
                onSaved={() => void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })}
              />
            ) : null}
          </div>
        </>
      )}
    </>
  )
}

// Deposit via a saved card (POST /payment/deposit/{bp}/card, CollectRequest {cardId, amount}),
// via a new card (POST /payment/deposit/{bp} → PaymentIntent → Stripe/Airwallex card confirm), or by
// bank transfer (POST /payment/deposit/{bp} with the BankTransfer gateway → PENDING transfer +
// reference number; an operator approves it later and the account is credited).
function DepositCard({
  pid, bp, currency, defaultCardId, preferredGatewayId,
}: { pid: string; bp: string; currency: string; defaultCardId?: string; preferredGatewayId?: string }) {
  const qc = useQueryClient()
  const [amount, setAmount] = useState("50")
  const [cardId, setCardId] = useState("")
  const [newCardOpen, setNewCardOpen] = useState(false)
  const [methodChoice, setMethodChoice] = useState<"card" | "bank">("card")
  const [bankResult, setBankResult] = useState<DepositResponse | null>(null)

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

  const cardGw = gateways?.find((g) => g.addFunds && g.thirdParty !== "BankTransfer")
  const bankGw = gateways?.find((g) => g.addFunds && g.thirdParty === "BankTransfer")
  // Bank transfer is the only choice when no card gateway exists; otherwise the user picks.
  const method: "card" | "bank" = bankGw && (!cardGw || methodChoice === "bank") ? "bank" : "card"
  const gw = method === "bank" ? bankGw : cardGw
  // New-card deposits confirm client-side with the gateway's card element (Stripe / Airwallex).
  const newCardGws = cardGateways(gateways, "addFunds")
  const minDeposit = Number(gw?.minDeposit ?? 0)

  const bankDeposit = useMutation({
    mutationFn: () =>
      apiFetch<DepositResponse>(`/payment/deposit/${bp}`, {
        method: "POST",
        body: { amount: Number(amount), paymentGatewayId: bankGw?.id },
      }),
    onSuccess: (resp) => {
      setBankResult(resp)
      void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })
      void qc.invalidateQueries({ queryKey: ["bank-transfers", bp] })
    },
    onError: (e: Error) => toast.error(e.message),
  })

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

        {bankGw && cardGw ? (
          <div>
            <Label className="mb-2 block">Payment method</Label>
            <Select
              value={method}
              onValueChange={(v) => {
                setMethodChoice(v as "card" | "bank")
                setBankResult(null)
              }}
            >
              <SelectTrigger className="w-full" aria-label="Payment method">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="card">Card</SelectItem>
                <SelectItem value="bank">Bank transfer{bankGw.name ? ` · ${bankGw.name}` : ""}</SelectItem>
              </SelectContent>
            </Select>
          </div>
        ) : null}

        {method === "bank" && bankGw ? (
          bankResult ? (
            <>
              <BankTransferPanel transfer={transferFromDeposit(bankResult)} gateway={bankGw} bp={bp} currency={currency} onCancelled={() => setBankResult(null)} />
              <Button variant="outline" onClick={() => setBankResult(null)}>
                Register another transfer
              </Button>
            </>
          ) : (
            <>
              <Button onClick={() => bankDeposit.mutate()} disabled={amountInvalid || bankDeposit.isPending}>
                {bankDeposit.isPending ? "Registering…" : "Pay by bank transfer"}
              </Button>
              <p className="text-xs text-muted-foreground">
                You will get a reference number and the bank details to transfer the amount (incl. tax). Your
                account is credited once the transfer is confirmed.
              </p>
            </>
          )
        ) : (
          <>
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
                disabled={!newCardGws.length || amountInvalid}
              >
                Deposit with new card
              </Button>
            </div>
            {!gw && gateways ? (
              <p className="text-xs text-muted-foreground">No payment gateway is configured for deposits.</p>
            ) : null}
          </>
        )}

        {/* Earlier transfers still awaiting the customer's "I have transferred" / our confirmation. */}
        <PendingBankTransfers bp={bp} gateway={bankGw} currency={currency} exclude={bankResult?.externalPaymentId} />

        {newCardGws.length ? (
          <NewCardDepositDialog
            open={newCardOpen}
            onOpenChange={setNewCardOpen}
            bp={bp}
            amount={amountNum}
            currency={currency}
            gateways={newCardGws}
            preferredGatewayId={preferredGatewayId}
            onDone={() => void qc.invalidateQueries({ queryKey: ["billing-summary", pid] })}
          />
        ) : null}
      </CardContent>
    </Card>
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
function BillingDetailsCard({ bp, activated = false, onSaved }: { bp: string; activated?: boolean; onSaved: () => void }) {
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
      toast.success(activated ? "Billing details saved" : "Billing details saved — profile activated")
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
          {activated
            ? "Your billing profile is active. Add a billing address for your invoices."
            : "Fill in your billing address to activate this billing profile and unlock deposits."}
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
