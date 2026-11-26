# Functions

**Platform → Functions** runs a container image of yours behind a stable HTTPS URL. A function stops when no request has arrived for a while and starts again on the next request, so it costs nothing while it is idle. It is meant for small HTTP services: webhooks, internal APIs, model front ends.

Functions are offered only where your operator has enabled them. If the menu has no **Functions** entry, the project has no location that runs functions: ask your operator.

A function runs an image that you name. The platform does not build images, and it has no registry of its own. A function's URL is **internal**: it answers on the platform's network, not on the public internet. Public URLs, hostnames of your own and GPUs are not offered.

## Before you rely on a function

The page shows these notes below the list. Read them once.

- **Who can call it.** There is no login in front of a function. Whoever can open a connection to the platform's function address and knows the URL can call it, and the six characters after the name are not a secret. A machine of yours (a server or a Kubernetes cluster of your project) reaches it where your operator routes your network to that address; a workstation reaches it over a network the site routes there. If callers must prove who they are, check a header or a token in the function itself. On most installations a sandbox or another function cannot call it, because their outbound traffic to the platform's private addresses is blocked.
- **Callers cost you.** Every request keeps the function awake, and under load it runs up to its **Maximum instances**. Whoever sends the requests, your project pays for the instances that run. A new function runs one instance at most. Raise the maximum only when you need it.
- **The certificate.** The URL is served with the platform's own certificate. Where your operator did not install a publicly trusted one, a client must trust the platform's certificate authority before it can call the function. Ask your operator for it.
- **Cookies.** Every function of every project is served under one parent domain. Never set a cookie with that parent as its `Domain`: every other function, of any project, would receive it. Leave the `Domain` attribute out, so a cookie stays with your function's own host.
- **Secrets and logs.** A function can read its secrets, so it can print them, and what it prints is in its logs. Every member who can view the project's resources can read the logs, read-only members too. See **Secrets** below for how a secret is kept.

## Creating a function

1. Select **New function**.
2. Give it a **Name**: lowercase letters, digits and `-`, starting with a letter, at most 40 characters. The name starts the function's URL and cannot be changed later.
3. Enter the **Image**. The registry must be named by a DNS name, not by an IP address and not as `localhost`, and the platform's machines must be able to reach it. Prefer a digest (`repository@sha256:…`), see **Tags and digests** below.
4. Choose **CPU** and **Memory (MiB)**, and the **Port** your server listens on (8080 by default).
5. Optionally add **Environment variables**, one `NAME=value` per line. `PORT` is set for you from the port, so you cannot set it yourself.
6. Optionally add **Secrets** (see **Secrets** below).
7. If the image is in a registry that needs a login, fill in **Private registry** (see **Private registry** below).
8. Optionally change **Stop after idle (seconds)**, and raise **Maximum instances** (see **Scaling on load** below).
9. Select **Create function**.

The status is **Deploying** until one instance answers on the port, then **Ready**. The **URL** column shows the address, for example `https://hello-api-3fa9c1.functions.example.com`, with a button that copies it.

What your image must do:

- Serve HTTP on the port you gave, on all interfaces (`0.0.0.0`), not only `localhost`.
- Start quickly. A deployment that is not ready within about two minutes fails.
- Keep no state on disk. Files are lost whenever the instance stops, and an instance has 1 GiB of disk for its own files.

## Tags and digests

A tag can move: `hello:1` today is not always `hello:1` tomorrow.

- A function deployed **by tag** pulls the tag again whenever an instance starts. A later instance, after an idle stop or under load, can therefore run another image than the first one.
- A function deployed **by digest** always runs exactly that image.
- A **rollback** always deploys the image that the earlier deployment actually ran, by its digest, also where that deployment named a tag. The **Deployments** list shows that digest.

## Statuses

| Status | Meaning |
|---|---|
| Deploying | A new deployment is rolling out |
| Ready | One or more instances are running and serving requests. With more than one, the list shows how many |
| Idle | No instance is running. The next request starts one |
| Suspended | Stopped by you. Requests get a 503 until you resume it |
| Failed | The first deployment did not become ready, so there is nothing to serve |

## Scale to zero and cold starts

After **Stop after idle (seconds)** without a request, the instance stops and the function becomes **Idle**. The next request starts a new instance and waits for it, so that request is slower (a cold start). If the instance is not ready in about 50 seconds, the caller gets a 503 with `Retry-After` and should try again. That happens, for example, when the image is large and must be pulled first, or when the platform has no room for the instance at that moment. The platform holds only so many waiting requests for one function: when that many already wait for it to start, a further request gets the 503 at once.

Where your location allows it, the dialog has **Keep one instance running**. It removes cold starts, but the function is then billed all the time.

## Scaling on load

A function runs one instance unless you set **Maximum instances** above 1. Then it runs more instances when it has many requests at the same time:

- **Requests per instance** is how many requests one instance is meant to handle at the same time (50 unless you change it). The function runs one instance for every so many requests in flight, up to **Maximum instances**. With 10 requests per instance and 25 requests in flight, it runs 3 instances.
- **Instances stop one at a time**, each after about a minute in which the load did not need all of them. The last instance stops only after **Stop after idle**.
- **An instance that stops may still have requests.** It gets no new request, and 3 seconds later the signal to stop (`SIGTERM`); 10 seconds after that it is ended. A request, stream or WebSocket on it that takes longer is cut. If your requests are long, finish them when the signal arrives, or keep **Maximum instances** at 1.
- **Requests per instance is a target, not a limit.** An instance is never refused a request. Set a number your server handles well.
- **Every running instance is billed**, whoever sends the requests. Anyone who can reach the URL can drive the function to its maximum.

Your instances share nothing: keep state that must survive between requests outside the function.

## Deploying a new version

**Deploy new version** (in the row's menu) changes the image, the private registry credentials, size, port, environment variables or secrets. Each change rolls out as a new **deployment**:

- The current instance keeps serving until the new one is ready, then traffic moves over.
- If the new deployment does not become ready, the function stays on its current one. The row shows **last deploy failed** and the details show the reason. The reason is one of a fixed set of sentences, never the registry's own answer.
- A function whose very first deployment failed has nothing to fall back to and shows **Failed**. Fix the cause and choose **Deploy new version**.
- One deployment runs at a time. Wait for it to finish before starting the next.

Changing only **Stop after idle**, **Keep one instance running**, **Maximum instances** or **Requests per instance** applies without a new deployment.

## Deployments and rollback

**Details and deployments** → **Deployments** lists the 10 newest deployments with their image, size and outcome. **Roll back** on an earlier deployment deploys it again, with the size, port and environment variables it had and the image it ran. The rollback appears in the list as a new deployment.

A rollback does not bring back old secrets or old registry credentials. It runs with the function's current secrets and pulls the image with the current credentials.

If an earlier deployment's image is in another registry than the stored credentials are for, its button reads **Roll back without credentials**: the credentials cannot pull that image, so the rollback removes them. That is right for a public image. If that image needs a login of its own, use **Deploy new version** and enter the image and its credentials together.

## Logs

**Logs** shows the newest 200 lines the running instance wrote to standard output and standard error. An idle function has no running instance, so it has no logs to show. Logs of stopped instances are not kept.

Every member who can view the project's resources can read the logs. Never print a secret.

## Suspend, resume and delete

- **Suspend** stops the function and keeps it stopped: requests get a 503 and do not start it.
- **Resume** lets the next request start it again.
- **Delete** stops the function and removes its deployment history. Its URL stops answering and is not given out again.

## Billing

A function is billed per minute for the CPU and memory of each instance, while at least one instance runs.

- Every running period is rounded up to whole minutes, and is at least one minute.
- The idle wait before the function stops is billed: the instance is still running.
- The start of an instance that went on to serve is billed from the moment it was started.
- While a deployment replaces the instances, the larger of the two sizes is billed.
- Nothing is billed while the function is **Idle** or **Suspended**.
- A start that never becomes ready (the image cannot be pulled, the platform has no room, the program crashes at start) is stopped within about two minutes and bills nothing.

A function is one line on the monthly bill. The minutes on it are the minutes in which at least one instance ran, and the amount is added up minute by minute at the number of instances that ran then. Multiplying the minutes by the size shown does therefore not always give the amount.

## Limits

The **New function** dialog shows your location's limits: the largest size, and how many functions a project may have. Your operator can also limit the functions, vCPUs and memory of a project; the limit counts every function at its **Maximum instances**, running or not.

## Secrets

A secret is an environment variable whose value nobody can read back in the console or through the API: not you, not other members. Use secrets for passwords, tokens and keys, and plain environment variables for everything else.

To add one, type its name and value under **Secrets** and select **Add secret**. The value field is a password field and is emptied as soon as the secret is added. After that the secret is listed by name only.

- **Replace** puts the name back in the form so that you can type a new value.
- **Remove** takes the secret away from the function.
- A name cannot be both an environment variable and a secret, and `PORT` cannot be a secret.

You find the same section in the **New function** and **Deploy new version** dialogs, and under **Details and deployments** → **Secrets**, where **Save and deploy** applies your changes. Changing secrets rolls out a new deployment. If it does not become ready, the function keeps the secrets it had.

What to know:

- **A value cannot be viewed again.** If you lose it, replace it.
- **Secrets have no history.** A rollback uses the current secrets, not the ones the earlier deployment ran with.
- **The function can print them.** Its logs are readable by every member who can view the project's resources.
- **How a secret is kept.** The portal stores it encrypted. On the platform's cluster it is a Kubernetes Secret, which is not encrypted there. Where the platform runs functions of several projects on the same machines, a break-out of the sandbox runtime on a machine could read the secrets of the functions running on that machine. Ask your operator how functions are placed if that matters to you.

## Private registry

If your image is in a registry that needs a login, give the function credentials under **Private registry**, in the **New function** dialog or under **Deploy new version**:

- **Server**: leave it empty to use the registry of the image. The field shows that registry as a hint. An image on Docker Hub uses `https://index.docker.io/v1/`. If you type it, write the host exactly as it is in the image name, including upper and lower case.
- **Username** and **Password**. An access token works as the password, and a token that can only read is the right choice.

The password field is never filled in for you. After saving, the section shows the server and the username only:

- **Replace** lets you enter new credentials. The password is needed every time, also if you change only the username, because nobody can read the stored one back.
- **Remove** takes the credentials away. **Keep** undoes Replace or Remove before you deploy.

Setting, replacing or removing credentials rolls out a new deployment. If the image cannot be pulled with them, the deployment fails with **image pull failed** and the function keeps the credentials it had.

What to know:

- **The password cannot be viewed again.** If you lose it, replace it.
- **The credentials have no history.** A rollback pulls with the current credentials.
- **The platform does not verify the credentials.** A deployment that works does not prove them: a public image can deploy also with a wrong username or password. To check your credentials, deploy an image that is really private.
- **One registry per function.** If you change the image to one in another registry, replace or remove the credentials in the same deployment. Otherwise the deployment is refused. **Replace** then leaves the server empty, so it follows the new image.

## Environment variables are visible to project managers

Members who may manage the project's resources can read the values of a function's environment variables. Other members see only the names. Put anything confidential in a secret instead.

## See also

- [Functions API](/docs/api/functions) for the routes behind this page.
- [How metering and billing work](/docs/concepts/billing-and-metering).
