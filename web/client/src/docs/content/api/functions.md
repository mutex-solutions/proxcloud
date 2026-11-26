# Functions API

This reference covers the project routes behind **Platform → Functions**, under the configured ProxCloud API prefix (normally `/api/v1`). The [Functions guide](/docs/guides/functions) describes the controls.

Functions are offered only on a platform release that has them, and there only where the operator has enabled them. On a release without them these routes do not exist and answer **404**. A function runs a container image that you name, behind an internal HTTPS URL. Builds from source, public URLs, hostnames of your own and GPUs are not offered: a request that names one is refused with **400**, not ignored.

## Basics

| | |
|---|---|
| Auth | A signed-in session or an API token, as for every project route. The project in the URL is authoritative |
| Permissions | List, get, deployments and logs need `project:cloud_resource:read`. Everything else needs `project:cloud_resource:manage` |
| Content type | `application/json` |
| Answers | The normal ProxCloud data envelope. The shapes below are its `data` value |
| Ids | `{id}` is the project id. `{fn}` is the function id: `fn-` and 16 hexadecimal digits |
| Times | RFC 3339, UTC |

| Route | Purpose |
|---|---|
| `GET /project/{id}/functions` | list |
| `POST /project/{id}/functions` | create and deploy |
| `GET /project/{id}/functions/{fn}` | get one, with what an unfinished rollout is waiting for |
| `PATCH /project/{id}/functions/{fn}` | change it |
| `DELETE /project/{id}/functions/{fn}` | delete |
| `GET /project/{id}/functions/{fn}/deployments` | the 10 newest deployments |
| `POST /project/{id}/functions/{fn}/rollback` | roll back to an earlier deployment |
| `POST /project/{id}/functions/{fn}/action` | `suspend` or `resume` |
| `GET /project/{id}/functions/{fn}/logs` | logs of the running instances |

## Where functions are offered

`GET /project/{id}/service` lists the project's services. The sandbox service carries a `functions` value only where functions are enabled. The console shows its menu entry and page on exactly that value.

```json
"functions": {
  "domain": "functions.example.com",
  "minInstances": 0, "maxMinInstances": 0,
  "idleSeconds": 300, "minIdleSeconds": 60, "maxIdleSeconds": 3600,
  "maxCpuMillis": 2000, "maxMemoryMi": 4096, "maxFunctions": 20,
  "maxInstances": 5, "defaultConcurrency": 50, "maxConcurrency": 1000
}
```

Values are examples, not fixed platform limits. A limit that is not stated has the value shown here. `minInstances` is the location's floor (`1` keeps one instance of every function running), `maxMinInstances` the highest `minInstances` a function may ask for, and `maxInstances` the highest `maxInstances` a function may set.

Where functions are not offered, every route that creates or changes something answers **403** `functions are not offered here`. The `GET` routes and `DELETE` still answer, so that a function left from before can be read and removed. Where the platform is not set up for functions at all, the routes answer **503** `functions are not configured`.

## The function object

```json
{
  "id": "fn-3b1f0c9a7d2e4f60",
  "name": "hello-api",
  "image": "registry.example.com/acme/hello@sha256:<64 hexadecimal digits>",
  "port": 8080,
  "cpuMillis": 250,
  "memoryMi": 256,
  "env": { "LOG_LEVEL": "info" },
  "secretNames": ["DB_PASSWORD"],
  "secretsSetAt": { "DB_PASSWORD": "2026-11-14T09:00:00Z" },
  "registryAuth": { "server": "registry.example.com", "username": "bot", "setAt": "2026-11-14T09:00:00Z" },
  "minInstances": 0,
  "idleSeconds": 300,
  "maxInstances": 1,
  "concurrency": 50,
  "host": "hello-api-3fa9c1.functions.example.com",
  "url": "https://hello-api-3fa9c1.functions.example.com",
  "status": "IDLE",
  "instances": 0,
  "suspended": false,
  "currentDeployment": { "id": "…", "seq": 2, "image": "…", "deployedAt": "2026-11-14T09:00:08Z" },
  "lastActiveAt": "2026-11-15T16:20:00Z"
}
```

- `name`: `^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`, unique in the project, never changed. The host is the name, `-`, six hexadecimal digits and the location's domain. A host is never given out twice.
- `image`: any reference the platform's machines can pull. The registry must be a DNS name, not an IP address and not `localhost`.
- `cpuMillis`: `250`, `500`, `1000`, `2000`, then whole vCPUs, up to the location's limit. `memoryMi`: `256` up to the limit.
- `env`: values are blank for a caller who may not manage the project's resources. `PORT` is reserved: it is set from `port`.
- `maxInstances`: `1` unless the request sets it. `concurrency` is the number of requests in flight one instance is meant to take: a scaling target, not a limit.
- `status`: `DEPLOYING`, `READY`, `IDLE`, `SUSPENDED` or `FAILED`. `reason`, on a single-function read, says what an unfinished rollout is waiting for.
- `lastError`: why the newest deployment failed. It is one of a fixed set of sentences, never the registry's or the cluster's own text.

### Secrets

Secrets are write-only. A request carries `secrets`, an object merged into what is stored: a string sets or replaces that name, `null` removes it, names left out stay.

```json
{ "secrets": { "API_TOKEN": "…", "OLD_TOKEN": null } }
```

No route returns a value: the function object lists `secretNames` and `secretsSetAt` only. A secret is given to the function as an environment variable; a name cannot be both in `env` and a secret, and `PORT` cannot be a secret. A change of secrets rolls out a new deployment. Secrets are not versioned: a rollback runs with the current ones.

A request body that cannot be read is refused without being echoed, because it may hold a secret. Audit records carry secret names, never values.

### Private registry

`registryAuth` is `{ "server", "username", "password" }`. `server` is optional and defaults to the registry of the image. The password is write-only and is needed whenever `registryAuth` is sent. `null` removes the stored credentials; leaving the field out keeps them. The function object returns `server`, `username` and `setAt`, never the password.

The credentials must be for the registry of the image. A request that changes the image to another registry must replace or remove them in the same request, or it is refused. The platform does not verify the credentials: a deployment that cannot pull fails, and says so with a fixed sentence.

## Create

`POST /project/{id}/functions` takes `name` and `image`, and optionally `port` (8080), `cpuMillis` (250), `memoryMi` (256), `env`, `secrets`, `registryAuth`, `idleSeconds`, `minInstances` (0), `maxInstances` (1) and `concurrency`. It answers the function object with `status` `DEPLOYING`; poll the function until the status changes.

## Update

`PATCH /project/{id}/functions/{fn}` takes the same fields except `name`. Send only what changes. `env` replaces the whole map; `secrets` is merged.

A change of `image`, `port`, `cpuMillis`, `memoryMi`, `env`, `secrets` or `registryAuth` rolls out a new deployment. If it does not become ready, the function stays on its current deployment and `lastError` says why. `idleSeconds`, `minInstances`, `maxInstances` and `concurrency` apply without a deployment.

## Deployments and rollback

`GET …/deployments` returns the newest first:

```json
{
  "id": "…", "functionId": "fn-3b1f0c9a7d2e4f60", "seq": 2,
  "image": "registry.example.com/acme/hello:1.1",
  "imageDigest": "sha256:<64 hexadecimal digits>",
  "port": 8080, "cpuMillis": 250, "memoryMi": 256,
  "env": { "LOG_LEVEL": "info" }, "secretNames": ["DB_PASSWORD"],
  "rollbackOf": 1,
  "state": "ACTIVE",
  "createdAt": "2026-11-14T09:00:00Z", "finishedAt": "2026-11-14T09:00:08Z"
}
```

`state` is `DEPLOYING`, `ACTIVE`, `SUPERSEDED` or `FAILED`; a failed one carries `message`. `imageDigest` is the digest the deployment's first ready instance ran.

`POST …/rollback` takes `{ "deploymentId": "…" }` and deploys that deployment again as a new one. Only a `SUPERSEDED` deployment can be rolled back to. The rollback deploys the recorded digest (`repository@sha256:…`), also where the deployment named a tag, with the deployment's size, port and environment but the function's current secrets and registry credentials. It may carry `registryAuth` as an update does: `null` removes credentials that are for another registry than that image.

A function deployed by tag pulls the tag again whenever an instance starts, so a later instance of the same deployment can run another image. Deploy by digest to rule that out.

## Suspend and resume

`POST …/action` takes `{ "action": "suspend" }` or `{ "action": "resume" }`. A suspended function runs no instance, is not billed, and answers its callers 503.

## Logs

`GET …/logs?tail=200` answers `{ "logs": "…" }`: the newest lines the running instances wrote to standard output and standard error. An idle function has none. Members with read permission can read the logs, so a function must not print its secrets.

## Calling a function

The URL answers on the platform's network only, with the platform's certificate. There is no credential, no rate limit and no caller authentication in front of it: whoever reaches the address and knows the URL can call the function, and every call can start an instance that the project pays for. A function that needs authentication checks it itself.

A request to an idle function starts an instance and waits up to about 50 seconds for it. After that the caller gets **503** with `Retry-After: 2`. When many requests already wait for the same function to start, a further one gets that 503 at once. A suspended function answers 503 at once.

All functions of all projects share one parent domain. Do not set cookies with that parent as their `Domain`.

## Errors

| HTTP | When |
|---|---|
| 400 | An invalid field, a renamed function, a rollback target that is not `SUPERSEDED`, an unknown action, a name that is both an environment variable and a secret, registry credentials for another registry than the image |
| 400 | `exposure: "public"`, `gpu`, a hostname of your own or a source to build from: not offered |
| 403 | No permission on the project; functions are not offered here (create, update, rollback and the actions); the project or its billing profile cannot be billed |
| 404 | An unknown function or deployment, or another project's |
| 409 | The name is already used in the project; a deployment of the function is already in progress |
| 429 | The project has as many functions as it may have, or its function limits are used up |
| 503 | Functions are not configured; a secret or a registry password cannot be stored because the platform has no key for it (nothing is stored); the cluster cannot be reached |

No error message contains a secret value or a registry password.

## Billing

A function is billed per minute for the CPU and memory of each running instance. A running period is rounded up to whole minutes. The idle wait before a stop is billed, and a deployment that changes the size is billed at the larger of the two sizes while it rolls out; an idle or suspended function is not. A start that never becomes ready is stopped within about two minutes and bills nothing. See [the guide](/docs/guides/functions) for the details.
