# Projects

Manage cloud projects. Base path: `/admin-api/v1/projects`.

## The project object

| Field | Type | Description |
|---|---|---|
| `id` | string | Project id. |
| `name` | string | Display name. |
| `status` | string | Project status. Newly created projects are `DISABLED`; provisioning sets `ENABLED`. |
| `organization_id` | string | Owning organization. |
| `billing_profile_id` | string | The **effective** billing profile: the project's own, or the owning organization's when the project has none. |
| `provisioned_services` | array | Always present. Each entry has `service_id` and `openstack.openstack_project_id` (the backing OpenStack/Keystone tenant id). |
| `members` | array | Always present. Each entry has `sub`. |

## GET /admin-api/v1/projects

Lists projects, paginated.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `organization_id` | string | no | Filter by owning organization. |
| `billing_profile_id` | string | no | Filter by the project's own billing profile id. |
| `member_sub` | string | no | Only projects the given `sub` belongs to. |
| `status` | string | no | Exact status match (e.g. `ENABLED`, `DISABLED`). |
| `openstack_project_id` | string | no | Find the project backing a given OpenStack tenant id. |
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
          "openstack": {
            "openstack_project_id": "3e1a9f2b6c8d4e5f9a0b1c2d3e4f5a6b"
          }
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
| `provision` | array | no | Non-empty ⇒ provision onto the platform cloud immediately after creation (the Keystone tenant is created and the project becomes `ENABLED`). |

Response `200`: `{ "data": { ...project } }`.

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Malformed body, or provisioning failed. |
| `404 NOT_FOUND` | `Billing profile not found` |
| `501 NOT_IMPLEMENTED` | `provision` was supplied but no platform cloud is configured on this deployment. |

## POST /admin-api/v1/projects/{id}/provision

Provisions an existing project onto the platform cloud: creates the backing Keystone tenant and sets the project `ENABLED`. Returns the refreshed project, including its new `provisioned_services` entry.

No request body.

Response `200`: `{ "data": { ...project } }`.

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Provisioning failed. |
| `404 NOT_FOUND` | Project doesn't exist. |
| `501 NOT_IMPLEMENTED` | No platform cloud is configured on this deployment. |

> Note: per-user cloud credentials aren't granted at provision time; resources are managed under the project's tenant scope.
