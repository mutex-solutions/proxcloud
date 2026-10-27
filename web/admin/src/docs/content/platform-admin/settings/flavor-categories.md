# Flavor Categories

Nova hands you flavors as a flat, often untidy list. Flavor categories bundle them into named tiers — *General Purpose*, *Compute Optimized*, *High Frequency CPU*, and the like — which the client portal renders as tabs in the Hardware step of the create-server flow. Categories double as a visibility control: a flavor that isn't in any category is never offered to clients.

## Where to set them up

Go to **System > Instances** in the admin portal. Existing categories appear with their assigned flavors; make a new one with **Add category**.

## Category fields

| Field | Description |
|---|---|
| Name | The tab's display name in the client create-server form, e.g. `General Purpose`. |
| Description | Optional text explaining the tier to clients. |
| Baremetal category | A toggle marking the category as bare-metal-only (Ironic flavors). Bare-metal categories appear in the bare-metal creation flow rather than the server one. |
| Flavors | The Nova flavors assigned to the category, chosen from the synced flavor list. |

## The client's view

On the create-server page each category becomes a tab under **Hardware**; clients pick a category, then a flavor within it. Bare-metal categories instead surface on the bare-metal creation page.

## A few tips

- Treat categories as your curation layer: keep internal or deprecated flavors out of every category rather than deleting them in Nova.
- Put each flavor in exactly one category — duplicating it across tabs only muddles the picker.
- Keep names short, since they render as tab labels.
