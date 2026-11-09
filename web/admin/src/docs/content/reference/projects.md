# Projects

Manage cloud projects. Base path: `/admin-api/v1/projects`.

## The project object

| Field | Type | Description |
|---|---|---|
| `id` | string | Project id. |
| `name` | string | Display name. |
| `status` | string | Project status. Newly created projects are `DISABLED`; provisioning sets `ENABLED`, and so does a member opening one under an `ACTIVE` billing profile or with billing unconfigured. |
| `organization_id` | string | Owning organization. |
| `billing_profile_id` | string | The **effective** billing profile: the project's own, or the owning organization's when the project has none. |
| `provisioned_services` | array | Always present. One entry per provider binding: `service_id`, `provider` (`PROXMOX`, `CEPH_S3`, `RUSTFS`, `TRUENAS`, `POWERDNS`, `KAMAJI`, `DBAAS` or `SANDBOX`), `region`, and `pool` (the project's PVE pool; Proxmox bindings only). Empty fields are omitted. |
| `members` | array | Always present. Each entry has `sub`. |

## GET /admin-api/v1/projects

Lists projects, paginated.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `organization_id` | string | no | Filter by owning organization. |
| `billing_profile_id` | string | no | Filter by the project's own billing profile id. |
| `member_sub` | string | no | Only projects the given `sub` belongs to. |
| `status` | string | no | Exact status match (e.g. `ENABLED`, `DISABLED`). |
| `limit` | integer | no | Page size (default 50, max 500). |
| `marker` | string | no | Keyset cursor. |

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/projects?organization_id=665f2a1bb8d34a0012340001"
```

Response `200`:

```json
{
  "data": [
    {
      "id": "665f3b0cb8d34a0012340010",
      "name": "production",
      "status": "ENABLED",
      "organization_id": "665f2a1bb8d34a0012340001",
      "billing_profile_id": "665f2a1bb8d34a0012340002",
      "provisioned_services": [
        {
          "service_id": "665f0000b8d34a0012340099",
          "provider": "PROXMOX",
          "region": "default",
          "pool": "pc-665f3b0cb8d34a0012340010"
        }
      ],
      "members": [
        { "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d" }
      ]
    }
  ]
}
```

## GET /admin-api/v1/projects/{id}

Fetches one project. Response `200`: `{ "data": { ...project } }`.

Errors: `404 NOT_FOUND`.

## POST /admin-api/v1/projects

Creates a project. Without a `provision` block the project is stored as `DISABLED`, with no members and no provisioned services; with one, provisioning runs right after the save and the fully provisioned project is returned.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Display name. |
| `organization_id` | string | yes | Owning organization id. |
| `billing_profile_id` | string | no | Must reference an existing billing profile. |
| `provision` | array | no | Non-empty ⇒ provision onto the platform cloud immediately after creation: bootstrap onto the Proxmox provider (project pool, default security group, default quota), then set the project `ENABLED`. |

Response `200`: `{ "data": { ...project } }`.

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Malformed body, or provisioning failed. |
| `404 NOT_FOUND` | `Billing profile not found` |
| `501 NOT_IMPLEMENTED` | `provision` was supplied but no platform cloud is configured on this deployment. |

## POST /admin-api/v1/projects/{id}/provision

Provisions an existing project onto the platform cloud. It bootstraps the project onto the Proxmox provider (project pool, default security group, default quota) and sets the project `ENABLED`, ending an operator's disable. Returns the refreshed project, including its new `provisioned_services` entry. A `DISABLED` project that is already provisioned stays `DISABLED`: enable it from the console, which also resumes its VMs.

One provider failing does not fail the call: the answer is 200 when the project got onto another provider, with what failed under `provision_failures` (`service_id`, `message`, `at`), retried when the project is next opened. It is 400 when the project is on no provider.

No request body.

Response `200`: `{ "data": { ...project } }`.

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Provisioning failed. |
| `404 NOT_FOUND` | Project doesn't exist. |
| `501 NOT_IMPLEMENTED` | No platform cloud is configured on this deployment. |

> Note: no per-user credentials are created on the cluster. Members reach the project's resources through ProxCloud, which scopes every call to the project's pool.
