// Org members, roles, audit trails, and the account profile.
import { mockProfile } from "../enabled"

export const orgMembers = [
  { sub: mockProfile.sub, firstName: "Dev", lastName: "User", email: mockProfile.email, role: "OWNER" },
  { sub: "user-alice", firstName: "Alice", lastName: "Example", email: "alice@example.com", role: "ADMIN" },
  { sub: "user-bob", firstName: "Bob", lastName: "Example", email: "bob@example.com", role: "MEMBER" },
]

export const projectMembers = [
  { userId: "u-1", sub: mockProfile.sub, firstName: "Dev", lastName: "User", email: mockProfile.email, role: "OWNER" },
  { userId: "u-2", sub: "user-alice", firstName: "Alice", lastName: "Example", email: "alice@example.com", role: "MEMBER" },
]

export const orgRoles = [
  { id: "role-owner", name: "OWNER", description: "Full control of the organization", permissions: ["*"], expandedPermissions: ["*"], builtIn: true },
  { id: "role-admin", name: "ADMIN", description: "Manage projects and members", permissions: ["org:manage", "project:manage"], expandedPermissions: ["org:manage", "project:manage"], builtIn: true },
  { id: "role-member", name: "MEMBER", description: "Use assigned projects", permissions: ["project:use"], expandedPermissions: ["project:use"], builtIn: true },
  { id: "role-billing", name: "billing-auditor", description: "Read-only billing access", permissions: ["billing:read"], expandedPermissions: ["billing:read"], builtIn: false },
]

export const permissionMeta = [
  { key: "org:manage", description: "Manage organization settings and members", resourceType: "ORGANIZATION" },
  { key: "project:manage", description: "Create, modify and delete projects", resourceType: "PROJECT" },
  { key: "project:use", description: "Use project cloud resources", resourceType: "PROJECT" },
  { key: "billing:read", description: "View invoices and cost data", resourceType: "BILLING" },
]

const curatedAuditEvents = [
  { id: "evt-1", timestamp: "2026-07-10T09:15:00Z", action: "server.start", resourceType: "SERVER", resourceId: "res-server-003", resourceDisplayName: "worker-01", outcome: "SUCCESS", actor: { id: mockProfile.sub, displayName: "Dev User", type: "USER" } },
  { id: "evt-2", timestamp: "2026-07-09T16:40:00Z", action: "member.invite", resourceType: "PROJECT", resourceId: "prj-aurora", resourceDisplayName: "aurora-production", outcome: "SUCCESS", actor: { id: "user-alice", displayName: "Alice Example", type: "USER" } },
  { id: "evt-3", timestamp: "2026-07-08T11:05:00Z", action: "volume.create", resourceType: "VOLUME", resourceId: "res-volume-002", resourceDisplayName: "scratch-vol", outcome: "SUCCESS", actor: { id: mockProfile.sub, displayName: "Dev User", type: "USER" } },
  { id: "evt-4", timestamp: "2026-07-07T22:10:00Z", action: "server.delete", resourceType: "SERVER", resourceId: "res-server-099", resourceDisplayName: "old-worker", outcome: "FAILED", actor: { id: "user-bob", displayName: "Bob Example", type: "USER" } },
]

// Deterministic filler so the cursor-paged audit endpoints actually page
// (>2 pages at the console's limit=50) and "Load more" is exercisable in dev.
const auditActors = [
  { id: mockProfile.sub, displayName: "Dev User", type: "USER" },
  { id: "user-alice", displayName: "Alice Example", type: "USER" },
  { id: "user-bob", displayName: "Bob Example", type: "USER" },
  { id: "svc-scheduler", displayName: "proxcloud-scheduler", type: "SERVICE" },
]
const auditActions: Array<[string, string, string]> = [
  ["server.create", "SERVER", "worker-02"],
  ["server.stop", "SERVER", "worker-01"],
  ["volume.attach", "VOLUME", "scratch-vol"],
  ["network.update", "NETWORK", "aurora-net"],
  ["keypair.create", "KEYPAIR", "deploy-key"],
  ["image.upload", "IMAGE", "ubuntu-24.04"],
  ["role.update", "ROLE", "billing-auditor"],
  ["bucket.create", "BUCKET", "aurora-artifacts"],
]
const generatedAuditEvents = Array.from({ length: 116 }, (_, i) => {
  const [action, resourceType, resourceDisplayName] = auditActions[i % auditActions.length]
  return {
    id: `evt-gen-${i + 1}`,
    // Descending from just before the curated events, ~47 min apart.
    timestamp: new Date(Date.parse("2026-07-07T20:00:00Z") - i * 47 * 60_000).toISOString(),
    action,
    resourceType,
    resourceId: `res-gen-${i + 1}`,
    resourceDisplayName,
    outcome: i % 9 === 4 ? "FAILED" : "SUCCESS",
    actor: auditActors[i % auditActors.length],
  }
})

export const orgAuditEvents = [...curatedAuditEvents, ...generatedAuditEvents]

export const accountAuditEvents = orgAuditEvents.map((e) => ({
  ...e,
  actor: { displayName: e.actor.displayName, ipAddress: "203.0.113.77" },
}))

export const accountDetails = {
  id: "acct-1",
  sub: mockProfile.sub,
  createdAt: "2026-01-15T08:00:00Z",
  firstName: "Dev",
  lastName: "User",
  email: mockProfile.email,
  language: "en",
  customInfo: {},
}

export const myInvites: Array<{ token: string; projectId: string; projectName?: string; expiresAt?: string }> = []
