// Airwallex card element (@airwallex/components-sdk): the SDK script is initialised once per
// environment, then a "card" element is created and mounted into the given container.
import { createElement, init, type ElementTypes } from "@airwallex/components-sdk"

export type AirwallexEnv = "demo" | "prod"
export type AirwallexCard = ElementTypes["card"]

const inits: Partial<Record<AirwallexEnv, Promise<unknown>>> = {}

export async function loadAirwallexCard(env: AirwallexEnv, container: HTMLElement | string) {
  inits[env] ??= init({ env, enabledElements: ["payments"] }).catch((e: unknown) => {
    delete inits[env] // a failed script load must not poison later attempts
    throw e
  })
  await inits[env]
  const card = await createElement("card")
  card.mount(container)
  return { card, unmount: () => card.destroy() }
}
