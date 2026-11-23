# Kubernetes clusters

**Platform → Kubernetes** lists your managed clusters. The control plane is hosted for you; the worker nodes are servers in your project, grouped in **node groups** (a flavor and a count each).

## Creating a cluster

The create form starts on the lowest Kubernetes version your operator offers, with **High availability** (three control-plane replicas instead of one) off. Pick a newer version or turn high availability on yourself; ask your operator which versions they have run on this site.

A first cluster needs two things from you: a **name** and, for its node group, a **flavor** (the size of each worker). The rest has a default:

- **Network.** The worker nodes run on one of your project's networks and get fixed addresses there, 24 of them reserved. When your project has exactly one network that can hold them, the form has it selected. When it has none, **Create a network for this cluster** makes one with the defaults of **Network → Networks** and selects it. A network that hands out addresses by DHCP cannot hold a cluster's nodes: the form marks the ones it knows of, and the create is refused with "Worker nodes need a network without DHCP" for any such network, marked or not. The form cannot tell before it makes a network whether your site gives new networks DHCP: when the one it made turns out to have it, the form says so and names it, so that you can delete it. When every network of your project has DHCP, a new one would be the same, and only your operator can change that.
- **Node group.** One group named `workers` with one node. Flavors that exceed your quota cannot be picked.
- **Add-ons, allowed API CIDRs and OIDC** are optional.

The network needs a way to the internet (the default of a new network), and the nodes must reach the cluster's own API address, which your operator provides.

## Status while a cluster is built

A new cluster reads **Pending**, then **Progressing**, then **Ready**. While it is being built, the cluster page shows a checklist: the control plane, one line per node group and the configuration sync. Each node group line says how many nodes are ready and what the platform is doing for the others:

| You read | Meaning | What you can do |
|---|---|---|
| Waiting for the control plane. | The workers start once the control plane is up. | Wait. |
| Preparing the machine. / Creating the virtual machine. / Starting the virtual machine. | The normal steps. With many nodes on slow disks this takes a while. | Wait. |
| Creating the virtual machine failed; retrying automatically. | A step failed and is tried again. | Wait. |
| The machine runs but has not joined the cluster yet. | The server is up and its node has not registered. In the first minutes this is a normal step. | Wait. |
| The machine runs but has not joined the cluster: most likely it could not reach the cluster's endpoint. | After 15 minutes with **Needs attention**: the server did not join and does not try again by itself. The likely cause is that it did not reach the cluster's endpoint. The path between your network and the endpoint is the operator's; you cannot check it, and the server's own error is shown to the operator only. | Contact the operator. Once the cause is repaired, use **Replace** on that server in the cluster's instance list ([Replace a node](#replace-a-node)). |
| The node joined but is not Ready. | The node registered and reports not Ready. | Check it with `kubectl describe node`. |
| The host has no free capacity for this machine, and it is not retried. | The host that should run the server is full. | Reduce the count or pick a smaller flavor. To retry, reduce the count by one and raise it again. |
| Creating the virtual machine failed and is not retried. | The platform gave up on this server. | Reduce the count by one and raise it again, or delete and recreate the cluster. |
| The platform cannot reach the virtualization provider. | A problem on the operator's side. | Contact the operator. |
| The control plane is taking longer than expected. | The hosted API server did not come up in 15 minutes. | Wait, delete and recreate, or contact the operator. |

## Replace a node

**Replace** deletes one server of the cluster and creates a new one in its place. The button is on every row of the cluster's instance list, and next to a node group's status for each server of the group that needs attention (such a server is not always in the instance list). The new server joins the cluster again. Use it for a server that did not join, after the operator repaired the cause, or for a node you want rebuilt.

- The node count of the group and your quota stay as they are, and the new server runs on the same host.
- A node that had joined is drained first. What is on its own disk is lost; persistent volumes are kept.
- For the last Ready node of a node group the console asks a second time: its pods have nowhere to run in that group until the new node is Ready, which takes several minutes.
- A machine whose server was never created has no row in the list: reduce the group's count by one and raise it again, or contact the operator.

On a site that has it turned on, the platform also replaces a server that did not join by itself, after about 30 minutes, in a node group of two or more servers. It does not when several servers of a group failed together: that points to a cause only the operator can repair.

## Needs attention

An amber **Needs attention** chip appears on the list and on the cluster page when a node group or the control plane made no progress for some time (15 to 90 minutes, depending on the step and on how many servers are built at once), or hit an error that is not retried. The **Status** column of the node group table shows which group it is.

Needs attention is not a final state. The cluster keeps its status, nothing is deleted, and the chip goes away by itself when the cluster recovers. You can wait, change the node group as its line says, delete and recreate the cluster, or contact the operator, who sees the full detail of every server.

While the platform works on a cluster the page refreshes itself every 15 seconds; a cluster that needs attention is refreshed once a minute.

## Node groups and hosts

The workers of a node group run on the hosts that hold the node image. When you create a cluster or raise a group's count, the platform puts each new worker on a host with enough free memory, CPU and disk, and spreads a group over several hosts when it can. Workers that already run stay where they are.

- If the new workers do not all fit, nothing is changed and you get a message such as *Node group "workers": only 2 of 5 new workers fit*. Reduce the count, pick a smaller flavor, or contact the operator.
- When you lower a count, a worker that never became ready is removed first; otherwise the host with the most workers of the group gives one up.
- Each worker's node carries the label `topology.kubernetes.io/zone` with its host, so you can spread or pin pods with it.
- On a site whose storage is local to each host, a volume lives on the host of the worker that created it. If you scale a group below the number of hosts it runs on, every worker on at least one host is removed, and pods that use a volume on that host cannot start elsewhere. The **Edit node groups** dialog warns you before you apply such a change.

## Firewall around the nodes

Your operator can put a cluster's nodes behind the platform firewall. The cluster page then shows a line **Worker firewall** with a security group named `k8s-<cluster id>`, which you also find under **Network → Security groups**.

With it, inbound traffic to a node is dropped unless a rule of that group allows it. The group starts with what the cluster needs between its own nodes (the pod network, health checks, the kubelet, MetalLB), your LoadBalancer addresses from anywhere, and NodePorts (30000 to 32767) from your project's networks. Outbound traffic is not filtered.

- The rules are yours to change. Add one when something outside the cluster must reach a port on a node directly, for example a `hostNetwork` service. Removing one of the rules the cluster started with can break the cluster.
- The kubelet port of a node is not reachable from your other servers.
- You cannot attach another security group to a node, detach or delete this one, attach it to a server, or create a group whose name starts with `k8s-`. The group is deleted with the cluster.
- A node that was just added or replaced gets the firewall within about 15 minutes.

A cluster page without that line has no firewall around its nodes: every port of a node is reachable from your project's own networks, and from nowhere else.

## Load balancer addresses

A Service of type `LoadBalancer` gets an address. Which kind depends on the cluster, and the cluster page says it under **Load balancer**:

- **Public address (default)**: on a site that offers the platform's load balancer, a cluster created since v0.11.0 gives each such Service a public address. A Service with `loadBalancerClass: proxcloud.io/internal` gets a private address of the cluster's network instead.
- **Public address on request (class only)**: a cluster created earlier, after your operator enabled the load balancer for it. Services keep their private addresses; a Service with `loadBalancerClass: proxcloud.io/lb` gets a public one. A Service's class cannot change: to move one, delete it and create it again. It gets a new address.
- **Private addresses only**: the cluster has no platform load balancer. Each Service gets one of 8 addresses of the cluster's network.

What a public address costs: the cluster's first Service with one builds its load balancer, two small VMs billed as servers with their disks, kept until the cluster is deleted. Each Service then takes one public address, billed and counted in your quota. With **NGINX Gateway Fabric** on a cluster with public addresses, each Gateway makes its own Service, so each Gateway takes a public address.

The cluster page shows the load balancer once it exists: its two VMs and which one holds the addresses, and each Service with its address, its ports and their protocol (`443/HTTPS`, `53/UDP`), how many workers answer it and its state. A port that ends TLS names the Secret its certificate came from and when that certificate expires, with a warning from 30 days before. The page also says when the cluster last reported to the platform, and lists the addresses the cluster keeps. **Open the load balancer** shows the Services on the load balancer's own page, which you cannot edit or delete: the cluster's Services make it.

Your project's dashboard lists every public address in use under **Quota & usage**, with what holds it: each Service by its name and cluster, each address a cluster keeps, each load balancer and each server with a public IP. That is the list to read when a Service is refused with `QuotaExceeded`.

The address shows in `kubectl get service` once it is ready, and `kubectl describe service` lists Events: a Service the platform refuses says why there, for example `QuotaExceeded` (no public address left in your quota), `ProtocolNotSupported` (an SCTP port), `NoNodePort` or `TooManySourceRanges`. Idle connections are closed after an hour unless you set a timeout. A Service deleted gives its address back at once, unless it asks to keep it.

### A cluster made before v0.11.1

UDP ports, TLS from a Secret and the annotations below need the cluster on chart 0.1.19 or later. A cluster made earlier still runs the load balancer controller it was made with: a Service with a UDP port is refused there with `UDPNotSupported` ("For UDP, upgrade the cluster"), and no annotation but `proxcloud.io/lb-proxy-protocol` is read. The cluster page says so under **Load balancer**. Apply the **Platform update** offered on the cluster page: it moves the cluster to the current chart and with it to the current controller. If the page offers none, your operator has not made the new chart available on this site yet: ask them. Your Kubernetes version, your workloads and the Services that are served today do not change. A Service that was refused for its UDP port then gets a public address, which is billed and counted in your quota: delete the Services you do not want served first.

### TCP and UDP ports

A Service may have TCP and UDP ports, also both on one port number (DNS on 53). SCTP is not served. UDP differs from TCP in four ways:

- **Your pods see the load balancer's address, not the client's.** There is no PROXY header for UDP.
- **The health check is a datagram of random bytes** to the node port every 5 seconds, next to a ping of the worker. Kubernetes hands that datagram to one of your pods: a service that cannot ignore junk bytes sets `proxcloud.io/lb-enable-health-monitor: "false"`.
- **A worker that is down or unreachable leaves in about 10 seconds. A closed port is often not seen while there is traffic**: the only sign is the "port unreachable" message a worker sends back, and a worker under traffic sends only a few of those per second. With `externalTrafficPolicy: Local` the check is an HTTP request to the Service's health port instead, for UDP ports too, and only workers that run a pod of the Service get traffic.
- A **connection limit** on a UDP port counts per worker, not for the port as a whole, and a UDP port has no idle timeout.

`sessionAffinity: ClientIP` keeps a client address on one worker for the time the Service sets (three hours by default), for TCP and UDP. The load balancer forgets these choices when its other VM takes over, and clients are then spread afresh.

### Options

A Service sets its options with annotations named `proxcloud.io/lb-<option>`. No other name is read. An option the platform does not know raises the Event `AnnotationNotSupported`, a value it cannot read `InvalidAnnotation`; the Service is served without that option.

| `proxcloud.io/lb-` | Values | What it does |
|---|---|---|
| `proxy-protocol` | `true` or `v2`, `v1`, `false` | Your pods get the client's address in a PROXY header (TCP ports only). `true` is version 2. |
| `method` | `roundrobin`, `leastconn`, `source` | How traffic is spread over the workers. |
| `connection-limit` | 1 to 20000, `-1` for none | Open connections per port; per worker on a UDP port. |
| `timeout-client-data`, `timeout-member-connect`, `timeout-member-data` | 1000 to 86400000 (milliseconds) | The idle time of a client, how long a worker may take to accept a connection, and the idle time of a worker. TCP ports only. |
| `x-forwarded-for` | `true` | The TCP ports speak HTTP and add `X-Forwarded-For`. Not together with the PROXY header: the PROXY header then holds. |
| `enable-health-monitor` | `true`, `false` | `false` turns the health check off. Not obeyed with `externalTrafficPolicy: Local`. |
| `health-monitor-delay`, `health-monitor-timeout`, `health-monitor-max-retries`, `health-monitor-max-retries-down` | 1 to 300 s, 1 to 300 s, 1 to 10, 1 to 10 | How often a worker is checked, how long it may take, and how many good checks bring it back or bad ones take it out. The default is every 3 s, back after 2, out after 2. A UDP port is back at its first good check and out after 2 bad ones at least. |
| `node-selector` | `key=value, key` | Only workers with all of these labels get the Service's traffic. |
| `tls-secret` | one to four Secret names, between commas | The load balancer ends TLS with these certificates (next section). |
| `tls-ports` | TCP ports of the Service, between commas | Only these ports end TLS. Without it every TCP port does. |
| `backend-protocol` | `http`, `tcp` | What your pods are spoken to in behind a port that ends TLS. |
| `keep-address` | `true`, `false` | The Service's address stays with the cluster when the Service goes. |
| `internal` | `true`, `false` | `true` refuses the Service: it never gets a public address. |

### TLS from a Secret

```yaml
metadata:
  annotations:
    proxcloud.io/lb-tls-secret: shop-tls
```

The load balancer then ends TLS on the Service's TCP ports with the certificate of that Secret and speaks plain HTTP to your pods, adding `X-Forwarded-For` and `X-Forwarded-Proto: https`. With `proxcloud.io/lb-backend-protocol: tcp` it passes the bytes on as TCP instead. Nothing is encrypted again between the load balancer and your workers. It accepts TLS 1.2 and 1.3.

- The Secret is of type `kubernetes.io/tls`, in the Service's own namespace, with the certificate chain (your server's certificate first) and a key without a passphrase: RSA of 2048 bits or more, or ECDSA P-256 or P-384. At most 4 Secrets a Service and 16 different ones a cluster.
- With several Secrets, a client gets the certificate that holds the name it asks for; the first Secret answers every other client.
- **Whoever may create or change a Service in a namespace can have any TLS Secret of that namespace served on a public address**, as with an Ingress. Give the right to write Services in a namespace only to those who may use its certificates.
- **A renewed Secret is picked up by itself**, within seconds, and no connection is dropped. cert-manager works as it is.
- **A Secret that is missing, of another type or refused never opens the port without TLS.** A Service that is served stays exactly as it was, with the certificate it had, and the Event `InvalidCertificate` names the Secret and the reason. A Service that was never served gets no address. An expired certificate is served, with the Event `CertificateExpired`.
- The certificates of a cluster's Services are not on the **Certificates** page: that page holds the ones you upload for load balancers you make yourself. The cluster page shows each Secret in use and when its certificate expires.

**Where the private key is kept.** The platform reads the Secret from your cluster and stores it encrypted in its database, for this cluster's load balancer only. It is written to the load balancer's two VMs, in a file only the system administrator of that VM can read; you cannot log in to those VMs, snapshot them or back them up. Your provider's operators can reach the key: a backup of the platform holds the encrypted key and, next to it, the key that decrypts it, and a backup or snapshot of a load balancer VM made on the virtualisation host holds the file itself. This is the same trust you place in the provider for the disks of your workers. When no Service names a Secret any more, its certificate leaves the VMs and the database within minutes; copies in backups made before stay until those backups are deleted.

### Keeping an address

With `proxcloud.io/lb-keep-address: "true"`, a Service's public address stays with the cluster when the Service is deleted. The cluster page lists it under **Kept addresses**. A kept address is **still billed and counted in your quota**. A new Service of the same cluster gets it by naming it in `spec.loadBalancerIP`. A cluster keeps 5 addresses at most; **Release** on the cluster page gives one back, and all go with the cluster. Any other `spec.loadBalancerIP` is not obeyed: the Service gets an address of the pool and the Event `LoadBalancerIPIgnored`.

### If your manifests were written for another cloud's controller

Charts for load balancer Services on a private cloud are mostly written for the upstream cloud controller manager, called OCCM below. A Service behaves here as it does there, with these differences, all of them on purpose:

1. **One load balancer per cluster.** All Services share its two VMs; each still has an address of its own.
2. **No load balancer objects to name.** There are no ids, flavors or zones of load balancers, listeners and pools, so no annotation about them.
3. **Which Services are served.** Class `proxcloud.io/lb`, and on a cluster with public addresses by default also a Service without a class. OCCM serves only Services without a class.
4. **The health check is on** unless you turn it off. OCCM's is off unless asked for.
5. **Other defaults.** A check every 3 s, back after 2 good checks, out after 2 bad ones, and an idle timeout of an hour. OCCM: 5 s, a 3 s timeout, 1 and 3, and 50 s.
6. **TLS comes from a Secret** of the Service's namespace, not from a secret store of the cloud, and only TCP ports end TLS.
7. **`proxy-protocol: "true"` is version 2.** OCCM's is version 1: write `v1` to keep it.
8. **A port without a node port refuses the Service** (`NoNodePort`). OCCM makes a pool without members.
9. **`spec.loadBalancerIP` is obeyed only for an address the cluster keeps.** OCCM takes any free address of the project.
10. **No load balancer shared between Services** by an id.
11. **The method `SOURCE_IP_PORT` balances round robin**, with an Event.
12. **The finalizer** on a Service is `proxcloud.io/load-balancer`.
13. **No site-wide defaults** for the method or the check's figures.
14. **A node selector written `key = value`** selects the workers labelled `key=value`; the spaces are dropped.
15. **The forwarded-for header asked for next to the PROXY header does not stop the Service.** It is served with the PROXY header alone and a Warning Event.
16. **The annotations have this platform's names.** OCCM's annotations are not read: on a Service here they do nothing and raise no Event.
17. **OCCM's annotation for an internal load balancer is not recognised.** A Service that carries only that one gets a **public** address here. Write `proxcloud.io/lb-internal: "true"`, which refuses the Service, or use class `proxcloud.io/internal` for a private address.

To rename a manifest: take the part of OCCM's annotation name after the `/` and put `proxcloud.io/lb-` in front of it. Four names differ:

| OCCM's name ends in | Write |
|---|---|
| `/lb-method` | `proxcloud.io/lb-method` |
| `/keep-floatingip` | `proxcloud.io/lb-keep-address` |
| `/default-tls-container-ref` | `proxcloud.io/lb-tls-secret`, with the name of a Secret instead of a reference into the cloud's secret store |
| the internal load balancer annotation | `proxcloud.io/lb-internal` (difference 17) |

An annotation about TCP inspection, or about the id, address, network, subnet, port, flavor, zone, tags or host name of a load balancer, has no counterpart: leave it out. Written under `proxcloud.io/lb-` all the same, it raises `AnnotationNotSupported`.


