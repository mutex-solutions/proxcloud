# Networking

The **Network** group in the sidebar holds everything that connects your servers to each other and to the outside world: private networks, the routers and ports that link them, public addresses, firewalls, load balancers, and DNS.

## Private networks

Start under **Network → Networks**, where you create the private networks your servers attach to. A project with no network yet needs one before its first server can be launched. **Routers** connect those private networks to each other and to an external network for internet access, while **Ports** are the individual network interfaces that attach a server to a network — usually created for you when you launch, but manageable directly when you need a fixed interface.

## Reaching the internet

Servers live on private networks by default. To expose one publicly, allocate a **Floating IP** under **Network → Floating IPs** and associate it with the server (or a specific port). The create dialog lists the public networks enabled for your project — your operator may restrict which external networks a project can allocate from. Releasing the floating IP when you no longer need it stops it counting against your quota and bill.

## Firewalling with security groups

**Network → Security groups** are the firewalls around your instances. Each group is a set of allow rules for inbound and outbound traffic; attach one or more to a server to control which ports and sources can reach it. A common first rule is allowing SSH (port 22) from your own address.

## Load balancers

**Network → Load balancers** spread incoming traffic across a pool of servers, so a single public endpoint can front several backends for capacity and resilience. Point the balancer at the members that should serve the traffic and configure the listener for the port and protocol you're serving.

## DNS zones

**Network → DNS zones** let you serve DNS from within the platform — create a zone for your domain and add the records that map names to your floating IPs and load balancers.
