// Platform setup: the rows of the connection test with ?checks=all and the node bridges the admin route adds.
type Doc = Record<string, any>

// A stored, trusted connection. template-pool is a warning, as on a cluster where nobody ran the setup script.
export const setupChecks: Doc[] = [
  { id: "connect", status: "ok", message: "Reached Proxmox VE 9.2.1." },
  { id: "token", status: "ok", message: "Proxmox accepts the token proxcloud@pve!platform." },
  { id: "firewall-datacenter", status: "ok", message: "The datacenter firewall is on." },
  { id: "firewall-nodes", status: "ok", message: "Every online node runs the nftables firewall." },
  { id: "privileges:/", status: "ok", message: "The token holds the ProxCloudPlatform privileges on /." },
  { id: "privileges:/pool", status: "ok", message: "The token holds the ProxCloudPlatform privileges on /pool." },
  { id: "version", status: "ok", message: "Proxmox VE 9.2.1 is 9.2 or later." },
  { id: "template-pool", status: "warn", message: "The pool pc-templates does not exist: run the setup script, or the first template import creates it." },
]

// An unsaved connection to a self-signed certificate that is not pinned yet: nothing after connect can run.
export const untrustedChecks: Doc[] = [
  { id: "connect", status: "fail", message: "Proxmox connection failed: tls: failed to verify certificate: x509: certificate signed by unknown authority" },
]
export const presentedFingerprint =
  "3A:9C:51:0E:7B:22:D4:8F:61:AA:05:C3:9E:47:12:B8:6D:F0:33:5A:C8:91:2E:7D:44:B6:0F:A1:58:E3:9B:C2"

export const nodeBridges: Doc[] = [
  { node: "pve1", iface: "vmbr0", type: "bridge", cidr: "192.0.2.11/24", management: true },
  { node: "pve1", iface: "vmbr1", type: "bridge", management: false },
  { node: "pve2", iface: "vmbr0", type: "bridge", cidr: "192.0.2.12/24", management: true },
  { node: "pve2", iface: "vmbr1", type: "bridge", management: false },
]
