// The in-memory "database": mutable state seeded from fixtures so that
// create/delete/action flows behave statefully within a session.
import type { CloudResource, Project } from "@/lib/types"
import { seedCloudResources, bucketObjects, s3Keys } from "./fixtures/cloud"
import { projects } from "./fixtures/platform"
import { cards, promoCredits, savingsContracts } from "./fixtures/billing"
import { orgMembers, projectMembers, orgRoles } from "./fixtures/people"

let counter = 1000

export const db = {
  cloud: seedCloudResources() as CloudResource[],
  projects: structuredClone(projects) as Project[],
  bucketObjects: structuredClone(bucketObjects),
  s3Keys: structuredClone(s3Keys),
  cards: structuredClone(cards),
  promoCredits: structuredClone(promoCredits),
  savingsContracts: structuredClone(savingsContracts),
  orgMembers: structuredClone(orgMembers),
  projectMembers: structuredClone(projectMembers),
  orgRoles: structuredClone(orgRoles),

  nextId(prefix: string) {
    return `${prefix}-${++counter}`
  },
}
