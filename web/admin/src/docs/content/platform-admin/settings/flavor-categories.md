# Flavor Categories

Flavors come from ProxCloud's own flavor catalog under **System → Flavors (Proxmox)**. Each flavor sets vCPUs, RAM and root-disk size, plus an optional GPU model and count and a storage profile. As the catalog grows, it can become a long, flat list. Flavor categories bundle flavors into named tiers, such as *General Purpose*, *Compute Optimized* or *GPU*. The client portal shows the tiers as sections in the Hardware step of the create-server flow.

## Where to set them up

Go to **System → Instances** in the admin portal. Existing categories appear with their assigned flavors; make a new one with **Add category**.

## Category fields

| Field | Description |
|---|---|
| Name | The section's display name in the client create-server form, e.g. `General Purpose`. |
| Description | Optional text explaining the tier to clients. |
| Baremetal category | Not used on Proxmox; leave it off. |
| Flavors | The flavors assigned to the category, chosen from the flavor catalog. |

## The client's view

On the create-server page, each category that contains at least one available flavor becomes a section under **Hardware**, in category order. Clients pick a flavor from those sections.

If no category matches any available flavor (for example, before you have set up categories), the wizard falls back to one unlabeled list of every flavor, so the picker is never empty.

## A few tips

- Treat categories as your curation layer. Once categories are in use, a flavor that isn't in any category isn't offered. To retire a flavor for new servers, take it out of every category or deactivate it in the flavor catalog.
- Put each flavor in exactly one category. Duplicating it across sections only muddles the picker.
- Keep names short, since they render as section labels.
