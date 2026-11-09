// Stage 1 of images/build.py: clone the vendor-image base, install the template contract, convert.
// build.py passes everything but ssh_username through PKR_VAR_*.
packer {
  required_plugins {
    proxmox = {
      version = "1.2.4"
      source  = "github.com/hashicorp/proxmox"
    }
  }
}

variable "proxmox_url" { type = string }
variable "token_id" { type = string }
variable "token_secret" {
  type      = string
  sensitive = true
}
variable "node" { type = string }
variable "base_vmid" { type = number }
variable "vmid" { type = number }
variable "name" { type = string }
variable "tags" { type = string }
variable "storage" { type = string }
variable "bridge" { type = string }
variable "vlan" { type = number }
variable "build_ip" { type = string }
variable "build_gateway" { type = string }
variable "nameserver" { type = string }
variable "ssh_username" { type = string }

source "proxmox-clone" "os" {
  proxmox_url              = var.proxmox_url
  username                 = var.token_id
  token                    = var.token_secret
  insecure_skip_tls_verify = true
  node                     = var.node
  clone_vm_id              = var.base_vmid
  vm_id                    = var.vmid
  full_clone               = true
  vm_name                  = var.name
  template_name            = var.name
  template_description     = "ProxCloud OS template, see images/README.md"
  tags                     = var.tags
  pool                     = "pc-templates"
  cores                    = 2
  memory                   = 2048
  cpu_type                 = "host"
  os                       = "l26"
  scsi_controller          = "virtio-scsi-single"
  // The agent is not in the image until the provisioners ran; build.py turns the flag on afterwards.
  qemu_agent               = false
  cloud_init               = true
  cloud_init_storage_pool  = var.storage
  task_timeout             = "15m"

  network_adapters {
    model    = "virtio"
    bridge   = var.bridge
    vlan_tag = var.vlan == 0 ? "" : tostring(var.vlan)
  }

  ipconfig {
    ip      = var.build_ip
    gateway = var.build_gateway
  }
  nameserver = var.nameserver

  ssh_username = var.ssh_username
  ssh_host     = split("/", var.build_ip)[0]
  ssh_timeout  = "10m"
}

build {
  sources = ["source.proxmox-clone.os"]

  provisioner "shell" {
    execute_command = "sudo -n sh -eu {{ .Path }}"
    scripts         = ["scripts/base.sh", "scripts/cleanup.sh"]
  }
}
