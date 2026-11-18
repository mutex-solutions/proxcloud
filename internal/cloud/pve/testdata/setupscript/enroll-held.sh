# ProxCloud: the Proxmox VE identity of the portal token.
# Paste into a root shell on one node (the cluster shares it). Safe to run again.
(
set -eu
PC_USER='proxcloud@pve'
PC_TOKEN_NAME='m0'
PC_TOKEN='proxcloud@pve!m0'

# the two platform tokens have fixed ids. One that exists and was not made for this installation may be in use by
# something else: this command never deletes or changes it, and stops here, before it changes anything
PC_INSTALL='pi-0a1b2c3d'
PC_MARK='proxcloud:pi-0a1b2c3d'
pc_state() { # user token-name: absent, ours (the comment is PC_MARK) or foreign
  pveum user token list "$1" --output-format json 2>/dev/null | perl -MJSON::PP -e 'my ($n, $m) = @ARGV; my $l = eval { decode_json(join "", <STDIN>) }; for (@{ ref $l eq "ARRAY" ? $l : [] }) { next unless $_->{tokenid} eq $n; print((defined $_->{comment} && $_->{comment} eq $m) ? "ours" : "foreign"); exit } print "absent"' "$2" "$PC_MARK"
}
PC_STOP=''
PC_CAPI=$(pc_state 'capmox@pve' 'capi')
PC_CSI=$(pc_state 'csi@pve' 'csi')
if [ -n "$PC_STOP" ]; then
  echo 'Nothing was changed.'
  exit 1
fi

# role: created empty when missing, then the privileges are appended (a re-run adds new ones, removes none)
pveum role add 'ProxCloudPlatform' 2>/dev/null || true
pveum role modify 'ProxCloudPlatform' --append 1 --privs 'Sys.Audit Sys.Console Sys.Modify Pool.Allocate Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.Backup VM.Console VM.GuestAgent.Audit VM.GuestAgent.Unrestricted VM.Migrate VM.PowerMgmt VM.Replicate VM.Snapshot VM.Snapshot.Rollback SDN.Allocate SDN.Audit SDN.Use Datastore.AllocateSpace Datastore.AllocateTemplate Datastore.Audit Mapping.Audit Mapping.Use'

# pools: OS templates, and the management VMs the token must not reach
pveum pool add 'pc-templates' --comment 'ProxCloud templates' 2>/dev/null || true
pveum pool add 'proxcloud-system' --comment 'ProxCloud management (not tenant)' 2>/dev/null || true

# user and privilege-separated token. The secret is printed once: store it
pveum user add "$PC_USER" --comment 'ProxCloud portal' 2>/dev/null || true
pveum user token add "$PC_USER" "$PC_TOKEN_NAME" --privsep 1 || echo "The token $PC_TOKEN exists already: its secret was printed when it was created."

# ACLs, for the user and for the token: a privilege-separated token holds the intersection of the two
grant() { # path role propagate
  pveum acl modify "$1" --users "$PC_USER" --roles "$2" --propagate "$3"
  pveum acl modify "$1" --tokens "$PC_TOKEN" --roles "$2" --propagate "$3"
}
grant '/' 'ProxCloudPlatform' 0
grant '/nodes' 'PVEAuditor' 1
grant '/pool' 'ProxCloudPlatform' 1
grant '/pool/proxcloud-system' 'NoAccess' 1
grant '/sdn' 'ProxCloudPlatform' 1
grant '/storage' 'ProxCloudPlatform' 1
grant '/mapping/pci' 'ProxCloudPlatform' 1
grant '/storage/nas-templates' 'PVEDatastoreAdmin' 0

# managed services: roles, the seed pool, the users and tokens of CAPMOX and proxmox-csi on the platform, and
# their ACLs; the portal token may then mint the tokens of tenant clusters. No secret is printed: the secrets of the
# tokens made here go into the one PC-ENROLL line at the end
pveum role add 'ProxCloudCAPI' 2>/dev/null || true
pveum role modify 'ProxCloudCAPI' --append 1 --privs 'Sys.Audit Pool.Audit VM.Allocate VM.Audit VM.Clone VM.Config.CDROM VM.Config.CPU VM.Config.Cloudinit VM.Config.Disk VM.Config.HWType VM.Config.Memory VM.Config.Network VM.Config.Options VM.GuestAgent.Audit VM.Migrate VM.PowerMgmt Datastore.AllocateSpace Datastore.Audit SDN.Use'
pveum role add 'ProxCloudCSI' 2>/dev/null || true
pveum role modify 'ProxCloudCSI' --append 1 --privs 'VM.Audit VM.Config.Disk Datastore.Allocate Datastore.AllocateSpace Datastore.Audit'
pveum role add 'ProxCloudKubeMint' 2>/dev/null || true
pveum role modify 'ProxCloudKubeMint' --append 1 --privs 'Permissions.Modify'
pveum role add 'ProxCloudKubeToken' 2>/dev/null || true
pveum role modify 'ProxCloudKubeToken' --append 1 --privs 'User.Modify'
pveum pool add 'proxcloud-seed' --comment 'ProxCloud seed cluster (not tenant)' 2>/dev/null || true
pc_token() { # user token-name state: prints the secret of a token made now; a token of an earlier setup is marked
  case "$3" in
  absent) pveum user token add "$1" "$2" --privsep 1 --comment "$PC_MARK" --output-format json | perl -MJSON::PP -e 'print decode_json(join "", <STDIN>)->{value}' ;;
  foreign) pveum user token modify "$1" "$2" --comment "$PC_MARK" >/dev/null ;;
  esac
}
PC_CAPI_SECRET=''
PC_CSI_SECRET=''
pc_enroll() { # said last, when this subshell ends, also when a step fails after a token was made: a secret is shown once
  if [ -n "$PC_CAPI_SECRET$PC_CSI_SECRET" ]; then
    echo
    echo 'Paste this one line into step 2 of the Managed services page. It carries the new token secrets: treat it as a password.'
    printf 'PC-ENROLL:%s\n' "$(printf '1:%s:%s:%s:%s' "$PC_INSTALL" "$(date +%s)" "$PC_CAPI_SECRET" "$PC_CSI_SECRET" | base64 -w0)"
  else
    echo 'No token was made, so there is no line to paste.'
  fi
}
trap pc_enroll EXIT
pveum user add 'capmox@pve' --comment 'ProxCloud CAPI' 2>/dev/null || true
PC_CAPI_SECRET=$(pc_token 'capmox@pve' 'capi' "$PC_CAPI")
pveum user add 'csi@pve' --comment 'ProxCloud CSI' 2>/dev/null || true
PC_CSI_SECRET=$(pc_token 'csi@pve' 'csi' "$PC_CSI")
# the only users the portal token may create and delete tokens for. Its grant sits on this group, never on
# /access/groups, which reaches every user of the cluster (root@pam too)
pveum group add 'proxcloud-kube' --comment 'ProxCloud: users the portal mints cluster tokens for' 2>/dev/null || true
pveum user modify 'capmox@pve' --groups 'proxcloud-kube' --append 1
pveum user modify 'csi@pve' --groups 'proxcloud-kube' --append 1
pveum acl modify '/' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 0
pveum acl modify '/' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 0
pveum acl modify '/nodes' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/nodes' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-seed' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/proxcloud-seed' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/pool/pc-templates' --users 'capmox@pve' --roles 'PVETemplateUser' --propagate 1
pveum acl modify '/pool/pc-templates' --tokens 'capmox@pve!capi' --roles 'PVETemplateUser' --propagate 1
pveum acl modify '/sdn/zones' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/sdn/zones' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/storage' --users 'capmox@pve' --roles 'ProxCloudCAPI' --propagate 1
pveum acl modify '/storage/pcplat' --tokens 'capmox@pve!capi' --roles 'ProxCloudCAPI' --propagate 0
pveum acl modify '/storage/local' --users 'capmox@pve' --roles 'PVEDatastoreAdmin' --propagate 0
pveum acl modify '/storage/local' --tokens 'capmox@pve!capi' --roles 'PVEDatastoreAdmin' --propagate 0
pveum acl modify '/pool' --users 'csi@pve' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --users 'csi@pve' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/pool/proxcloud-system' --tokens 'csi@pve!csi' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/pool/proxcloud-seed' --users 'csi@pve' --roles 'NoAccess' --propagate 1
pveum acl modify '/storage' --users 'csi@pve' --roles 'ProxCloudCSI' --propagate 1
pveum acl modify '/storage/pcplat' --tokens 'csi@pve!csi' --roles 'ProxCloudCSI' --propagate 0
grant '/access/groups/proxcloud-kube' 'ProxCloudKubeToken' 0
grant '/storage' 'ProxCloudKubeMint' 1
grant '/sdn/zones' 'ProxCloudKubeMint' 1
grant '/sdn/zones' 'ProxCloudPlatform' 1
grant '/nodes' 'ProxCloudKubeMint' 0
grant '/pool/proxcloud-seed' 'NoAccess' 1
grant '/storage/nas-templates' 'ProxCloudKubeMint' 0
# a row of this user or token on a single store, zone or vnet replaces what /storage and /sdn/zones pass down there, the
# mint role too, and a tenant cluster on that store is then refused. The mint role is written next to each such row,
# for the one that holds the row (a row for the other would replace what that one inherits), propagated as the row is
PC_SHADOW=$(pveum acl list --output-format json | perl -MJSON::PP -e 'my ($u, $t) = @ARGV; my %p; for (@{ decode_json(join "", <STDIN>) }) { next if $_->{roleid} eq "ProxCloudKubeMint"; my $f = $_->{type} eq "user" && $_->{ugid} eq $u ? "--users" : $_->{type} eq "token" && $_->{ugid} eq $t ? "--tokens" : next; next unless $_->{path} =~ m{^/storage/[^/]+$|^/sdn/zones/[^/]+$} || ($_->{path} =~ m{^/sdn/zones/([^/]+)/[^/]+$} && $1 ne "localnetwork"); $p{"$f $_->{ugid}"}{$_->{path}} ||= $_->{propagate} ? 1 : 0 } for my $k (sort keys %p) { print "$k $p{$k}{$_} $_\n" for sort keys %{ $p{$k} } }' "$PC_USER" "$PC_TOKEN")
printf '%s\n' "$PC_SHADOW" | while read -r PC_FLAG PC_ID PC_PROP PC_PATH; do
  [ -z "$PC_PATH" ] || pveum acl modify "$PC_PATH" "$PC_FLAG" "$PC_ID" --roles 'ProxCloudKubeMint' --propagate "$PC_PROP" </dev/null
done
# a site set up before the group: the mint role sat on /access/groups and /pool and held User.Modify. These remove
# exactly this token's rows, after the grant on the group above, and change nothing on a new site
pveum acl delete '/access/groups' --users "$PC_USER" --roles 'ProxCloudKubeMint'
pveum acl delete '/access/groups' --tokens "$PC_TOKEN" --roles 'ProxCloudKubeMint'
pveum acl delete '/pool' --users "$PC_USER" --roles 'ProxCloudKubeMint'
pveum acl delete '/pool' --tokens "$PC_TOKEN" --roles 'ProxCloudKubeMint'
# User.Modify leaves the role for every holder at once, so only when no other portal token still creates tokens
# through a row on /access/groups: run this script for each of them, the last run takes the privilege out
PC_LEFT=$(pveum acl list --output-format json | perl -MJSON::PP -e 'print map { "  $_->{type} $_->{ugid}\n" } grep { $_->{path} eq "/access/groups" && $_->{roleid} eq "ProxCloudKubeMint" } @{ decode_json(join "", <STDIN>) }')
[ -n "$PC_LEFT" ] || pveum role modify 'ProxCloudKubeMint' --privs 'Permissions.Modify'
pc_left() { # said last, when this subshell ends, below everything the script prints
  echo 'NOT FINISHED: ProxCloudKubeMint still holds User.Modify, because these still hold that role on /access/groups:'
  echo "$PC_LEFT"
  echo 'Each is another portal token, or its user, that creates cluster tokens through that row and would stop if the role lost'
  echo 'User.Modify now. Run this script for each such token (tokenId=<its id>), or make upgrade for the token of an installer site;'
  echo 'the last run takes User.Modify out of the role. Until then a token listed here whose user is listed too can create an API'
  echo 'token for any user of the cluster, root@pam included. A row no portal uses: pveum acl delete /access/groups --users <user>'
  echo '(or --tokens <token id>, in single quotes) --roles ProxCloudKubeMint, then this script again.'
}
[ -z "$PC_LEFT" ] || trap 'pc_left; pc_enroll' EXIT
# what the platform tokens hold on the platform pools: the CAPI token ProxCloudCAPI on both, the CSI token nothing on the
# seed VM (its disks on the boot store stay open to the CSI token: docs/proxmox-permissions.md, "What the seed pool does not fence")
pveum user token permissions 'capmox@pve' 'capi' --path '/pool/proxcloud-system'
pveum user token permissions 'capmox@pve' 'capi' --path '/pool/proxcloud-seed'
pveum user token permissions 'csi@pve' 'csi' --path '/pool/proxcloud-system'
pveum user token permissions 'csi@pve' 'csi' --path '/pool/proxcloud-seed'

# what the token really holds
pveum user token permissions "$PC_USER" "$PC_TOKEN_NAME"
# the certificate fingerprint to pin on the provider: pve-ssl.pem, or pveproxy-ssl.pem when the node has its own
pvenode cert info
)
