#!/bin/sh
down() {
  sysctl -q -w net.ipv4.ip_forward=0
  nft delete table ip proxcloud-lb 2>/dev/null
  return 0
}
case "$1" in
up)
  modprobe -a ip_vs ip_vs_wrr ip_vs_lc ip_vs_sh 2>/dev/null
  if sysctl -q -p /etc/proxcloud-lb/lvs.sysctl && nft -f /etc/proxcloud-lb/lvs.nft && nft list chain ip proxcloud-lb lb-forward | grep -q 'policy drop'; then
    m=/proc/sys/net/netfilter/nf_conntrack_max
    [ "$(cat $m 2>/dev/null || echo 0)" -ge 262144 ] || { echo 262144 > $m; } 2>/dev/null || true
    sysctl -q -w net.ipv4.ip_forward=1 && exit 0
  fi
  echo "the kernel's IPVS or the nftables table could not be set up" >&2
  down
  exit 1 ;;
down) down ;;
*) exit 2 ;;
esac
