# Load balancers

A load balancer gives your servers one public address and spreads traffic across them. Servers that stop answering are left out until they answer again.

## What you pay for

A load balancer runs on its own small VMs in your project: two with high availability (the default), one without. Each costs what a server of that size costs, plus its disk, and the load balancer adds its public address and the traffic on that address. It counts in your quota the same way: two servers and one public address with high availability. The VMs do not appear under **Servers**; the load balancer's page lists them. You will see them, their disks and the address on your bill.

## Create one

Under **Network → Load balancers**, press **Create load balancer** and pick a name, the network your servers are on, a public network and a size. The network must be one whose addresses the platform assigns, which is the usual kind. **High availability** is on unless you turn it off.

A load balancer needs three things from the site that only your operator can provide: a public network allowed for your project, a free public address in your quota, and the image the load balancer runs on. The dashboard's **Quota & usage** card shows how many public addresses are in use and lists them with what holds each one (a server, a load balancer, a Service of a Kubernetes cluster). When one is missing, the page says which and **Create load balancer** is disabled, so you do not find out by a failed create. File shares have a similar notice when your project's networks cannot reach the file server.

The status reads `build` for a few minutes while the VMs boot and install what they need, then `active`.

## High availability

With high availability the load balancer runs on two VMs, called appliances. One holds the public address and carries the traffic; the other waits. When the active one fails, stops, or its HAProxy stops, the other takes the address over within a few seconds (about 4 seconds when a VM dies, under a second when one is stopped on purpose). Connections open at that moment are cut, and clients connect again.

The page lists both appliances, the node each runs on, and which one holds the address. The status reads:

- `active`: both answer and one holds the address.
- `degraded`: only one answers, or both hold the address because they cannot hear each other. The page says which. The load balancer keeps serving.
- `down`: neither answers.

Where both appliances run on the same node, which happens on a site with one node and on some networks, the page says so: the load balancer then survives the failure of an appliance, not the loss of the node.

A load balancer made before high availability existed, or with it turned off, runs on one VM. It cannot be changed into a pair: create a new load balancer (it gets a new address) and delete the old one.

### Replace appliances

**Replace appliances** builds both VMs again from the current image, one at a time, the standby first, so the address keeps answering. Traffic switches once, in about a second. Use it to get updated software, or to rebuild an appliance that is gone. It takes a few minutes per appliance, and the load balancer cannot be deleted while it runs.

## A Kubernetes cluster's load balancer

A load balancer named `k8s-…` whose row names a Kubernetes cluster belongs to that cluster: its Services of type `LoadBalancer` make it ([Kubernetes clusters](/docs/guides/kubernetes)). Its page lists those Services with their addresses, each port with its protocol (TCP, UDP, or TLS and HTTPS for a port that ends TLS) and how many workers answer, and links to the cluster. It has no listeners, pools or **Delete** for you to change: change the cluster's Services instead, and it is deleted with the cluster. **Replace appliances** works as on any load balancer.

What this page says about UDP, TLS and health checks holds for a cluster's Services too, with three differences. The options are set on the Service, with annotations ([Options](/docs/guides/kubernetes#options)). A certificate comes from a Secret of the cluster, not from the **Certificates** page, and is renewed by changing the Secret ([TLS from a Secret](/docs/guides/kubernetes#tls-from-a-secret)). And a UDP port of a Service has no payload and no required reply: its check is the datagram of random bytes with a ping, or the Service's health port with `externalTrafficPolicy: Local`.

## Add listeners, pools and servers

- A **pool** is a set of your servers. Choose how to spread traffic: round robin, least connections, or by source address.
- Each server in a pool has a **port** and a **weight**. A server with twice the weight gets twice the traffic.
- A **listener** opens a port on the public address and forwards it to a pool. Its **protocol** is TCP, TLS, HTTP, HTTPS or, on a load balancer with high availability, UDP. The pool speaks TCP behind a TCP or TLS listener, HTTP behind an HTTP or HTTPS listener and UDP behind a UDP listener: a pool serves listeners of one kind, so changing a listener's protocol can change its pool and the pool's other listeners with it. Under **Allowed sources** you can list the networks that may connect, such as `203.0.113.0/24`; empty means any address. Other clients are dropped before they reach the load balancer.
- A listener's **connection limit** is the most connections it holds open at a time; further clients wait until one closes. Empty means no limit of its own. The load balancer as a whole holds 20000. Its **idle timeout** closes a client's connection after that many seconds without data (60 when empty).
- **Session persistence** by source address keeps each client address on the server it first reached, until that address has made no new connection for the time you set (three hours when empty). The load balancer forgets these choices when you save a change and when the other appliance takes over, and clients are then spread afresh.
- **PROXY protocol** on a pool passes the client's address to your servers in a PROXY header, version 1 (a text line) or 2 (binary). Turn it on only when your servers expect that header, in that version. **Health checks carry the header too** when they go to the port that takes the traffic, so a server that accepts connections only with the header is checked correctly. A health check with a **port** of its own carries no header, also when that port is the same number: whatever answers there must take a plain connection. So leave the check's port empty for a server that expects the header on every connection, and give the check its own port only for a health endpoint that speaks without it.
- A pool's **connect timeout** is how long a server may take to accept a connection before the next server is tried (2 seconds when empty), and its **server idle timeout** closes a connection after that many seconds without data from the server (60 when empty). For long-lived connections such as a database or a websocket, raise both idle timeouts.

TLS and UDP have sections of their own below.

Press **Save configuration**. The change reaches the load balancer within seconds. If HAProxy refuses it, the page shows why and the load balancer keeps running the last configuration that worked.

Only servers of this project, on the load balancer's own network and subnet, can be added. HTTP pools add an `X-Forwarded-For` header with the client's address. To serve HTTPS, either let the load balancer end TLS (next section) or use a TCP listener on port 443 and end TLS on your servers.

## TLS and HTTPS

A **TLS** or **HTTPS** listener ends TLS on the load balancer, with a certificate you upload, and speaks plain to your servers:

- **HTTPS** speaks HTTP to the pool. Your servers read the client's address in `X-Forwarded-For` and the scheme in `X-Forwarded-Proto: https` (whatever the client sent in that header is replaced). Clients may use HTTP/2 or HTTP/1.1; your servers are spoken to in HTTP/1.1.
- **TLS** passes the bytes on as TCP, for any protocol over TLS. Turn on the pool's PROXY protocol if your servers need the client's address.

The connection from the load balancer to your servers is not encrypted: it stays on your project's network. If it must be encrypted too, use a TCP listener and end TLS on the servers.

Pick one or more certificates on the listener. A client gets the certificate that holds the host name it asks for; the **first** certificate answers every other name, and clients that ask for none. The load balancer accepts TLS 1.2 and 1.3 with modern ciphers only; this is not configurable.

### Certificates

Upload a certificate on the **Certificates** page: a name, the certificate chain as PEM text (your server's certificate first, then each intermediate; this is `fullchain.pem` from most tools) and its private key (RSA of 2048 bits or more, or ECDSA P-256 or P-384, without a passphrase). The page then shows the names the certificate is valid for, when it expires, its SHA-256 fingerprint and which load balancers use it. An expired certificate is accepted with a warning: clients will refuse it.

The page and each listener warn from 30 days before a certificate expires. Nothing renews a certificate for you.

### Replace a certificate

A certificate does not change after its upload. Before it expires:

1. Upload the new certificate under a new name.
2. On each listener that uses the old one, add the new certificate and take the old one off, then **Save configuration**.
3. Delete the old certificate. One that a listener still uses cannot be deleted.

The load balancer takes the change within seconds. Open connections are not closed; new connections get the new certificate.

### Where your private key is kept

- It is stored encrypted in the platform's database and no page or API call shows it again: keep your own copy.
- It is written to your load balancer's appliances, in a file only the system administrator of that VM can read. You cannot log in to an appliance, snapshot it or back it up.
- Your provider's operators can reach it: a backup of the platform holds the encrypted key and, next to it, the key that decrypts it, and a backup or snapshot of an appliance VM made on the virtualisation host holds the file itself. This is the same trust you place in the provider for the disks of your servers.
- Deleting a certificate removes it from the database; the appliances drop its file when their configuration no longer names it. Copies in backups made before stay until those backups are deleted.

## UDP

A load balancer with high availability also takes **UDP** listeners; one that runs on a single VM does not, and says so when you try. A UDP listener forwards to a UDP pool, and the same port number can hold a TCP listener and a UDP listener side by side (DNS on 53, for example).

- **Your servers see the load balancer's address, not the client's.** Each datagram reaches a server from the private address of the appliance that carries the traffic. There is no PROXY protocol for UDP, so nothing passes the client's address on.
- **Spreading traffic** works as for TCP: round robin, least connections or by source address, with the servers' weights. **Session persistence** by source address keeps a client address on one server until it has been silent for the time you set (three hours when empty).
- A UDP listener's **connection limit** counts per server, not for the listener as a whole: with a limit of 1000 and three servers, up to 3000 clients are served at a time. It has no idle timeout, and a UDP pool has no connect or server timeout.
- **After a failover** of the load balancer the other appliance spreads the clients afresh, so a client may reach another server than before. With traffic spread by source address it is the same server as long as both appliances see the same servers up.
- A UDP listener whose pool has no server that can take traffic answers "port unreachable".

### Checking UDP servers

UDP has no connection to open, so the checks are different, and it matters which one you choose:

- **UDP datagram** (the default) sends a datagram to the server's port every 5 seconds and pings the server. A server that is stopped or unreachable leaves the pool in about 10 seconds. **A server that runs with nothing listening on the port is often not taken out**: the only sign is the "port unreachable" message the server sends back, and a server under traffic sends only a few of those per second. A server whose firewall drops packets in silence is never taken out.
- The datagram holds **random bytes** unless you give a **payload**, so your service receives junk every few seconds and should ignore it. Give a payload (as hexadecimal digits, `70696e67` for `ping`) that your service understands.
- **Require a reply** is the real check: a server is up only while it answers the payload, and a closed port is seen within about 12 seconds. Use it whenever your service answers something.
- **TCP connection** or **HTTP GET** check another side of the same service, on the **check port** you name: the TCP port of a DNS server, a health endpoint next to a game server.
- **None**: no check, every server always gets traffic.
- **Every**, **Timeout** and **Down after** work as for TCP (5 seconds, 5 seconds and 3 checks when empty; **Down after** is 2 at least). There is no **Up after**: a server is back at its first good check. A server you add gets no traffic until its first good check.

## Let the traffic in

The load balancer reaches your servers from its private addresses (one per VM; a replace gives new ones). The `default` security group already allows that. If a server uses other groups, open its port to the load balancer's network, for UDP when the pool is a UDP pool. The default check of a UDP pool also pings the server: let ICMP echo in from the load balancer's network as well, or the server never enters the pool (a check with a required reply, a TCP check or an HTTP check sends no ping).

## Health

Every server of a TCP or HTTP pool is checked every 3 seconds, by default with a TCP connection to its port (a UDP pool's checks are described under UDP above). Two failures take a server out, two successes bring it back. The page shows each server's state as `up` or `down`. A pool can check differently:

- **HTTP GET** or **HTTPS GET** on a path you choose: any 2xx or 3xx answer is healthy. The HTTPS check speaks TLS to the server and does not verify its certificate.
- **TLS hello**: the server is healthy when it answers a TLS hello with TLS. Use it for a TCP pool whose servers end TLS themselves. It does not look at the certificate.
- **None**: no check. Every server always gets traffic and reads `no check`, also one that was down before you chose it; use it only when a check would disturb the service.
- On **another port** than the one traffic goes to.
- With your own figures: how often (**Every**, 1 to 300 seconds), how long a server may take to answer once the check is connected (**Timeout**; with one set, the connection itself must come within the pool's connect timeout), and how many checks in a row bring a server back (**Up after**) or take it out (**Down after**), 1 to 10 each.

If you delete a server that is in a pool, it stays listed as `gone` and receives no traffic until you remove it.

## While a project is suspended

The load balancer's VMs are paused with your servers, so it serves nothing and its configuration cannot change. You can still delete it. It comes back by itself when the project is resumed.

## Delete one

**Delete** removes the VMs and their disks and gives the address back. Your servers are not touched. You cannot delete its VMs or its address on their own, stop them, add a volume to them or make an image of them: change or delete the load balancer instead. While it is first being created, **Delete** asks you to try again a moment later.

Nothing updates the software on a running load balancer by itself. With high availability, press **Replace appliances**. A load balancer on one VM has no replace: create a new load balancer and delete the old one; the new one gets a different address.
