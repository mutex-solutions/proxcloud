package pve

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultPlatformTokenID is the portal token the docs and the installer use.
const DefaultPlatformTokenID = "proxcloud@pve!platform"

// What PlatformSetupScript accepts. Narrower than Proxmox itself: the values end up in a script a root shell runs.
var (
	setupTokenID   = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,63}@[A-Za-z][A-Za-z0-9._-]{0,31})!([A-Za-z][A-Za-z0-9._-]{0,63})$`)
	setupStorageID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)
)

// PlatformSetupScript is the shell script that creates the portal's identity on a cluster: the role with
// PlatformRolePrivileges, the template and system pools, the user, its privilege-separated token and PlatformACLs.
// It is meant to be pasted into a root shell on one node, and to be run again after an upgrade:
//   - everything runs in a subshell, so its `set -e` ends the script and not the operator's session;
//   - every value is single-quoted where it is set. A `!` outside single quotes is history expansion in an
//     interactive shell, and the token id has one;
//   - the role is created empty and its privileges appended, so a re-run adds new privileges and removes none,
//     and a pool, user or token that exists is left alone.
//
// gpu adds the GPUOnly ACLs. importStorage adds what image import needs on that store, where the portal deletes the
// files it uploaded (docs/proxmox-permissions.md).
func PlatformSetupScript(tokenID string, gpu bool, importStorage string) (string, error) {
	return setupScript(tokenID, gpu, importStorage, nil)
}

// ManagedSetupScript is PlatformSetupScript plus what managed services need: ManagedRoles, the pool SeedPool, the
// users capmox@pve and csi@pve with their tokens capi and csi, the group KubeMintGroup with both users in it,
// ManagedACLs (the three stores as there), and the effective rights of both tokens on both platform pools. Safe to run
// again, as PlatformSetupScript, with one exception to "removes none": it takes the mint role's rows of this token and
// its user off KubeMintOldPaths, which an earlier version of this script wrote, and User.Modify out of that role once
// no other principal holds the role on /access/groups. The role is the cluster's: taking the privilege out earlier
// would stop every other portal token that still creates tokens through its old row.
func ManagedSetupScript(tokenID string, gpu bool, importStorage, bootStorage, isoStorage string) (string, error) {
	for _, s := range [][2]string{{"bootStorage", bootStorage}, {"isoStorage", isoStorage}} {
		if !setupStorageID.MatchString(s[1]) {
			return "", fmt.Errorf("%s must be a storage id: a letter, then letters, digits, dot, dash and underscore", s[0])
		}
	}
	return setupScript(tokenID, gpu, importStorage, ManagedACLs(tokenID, bootStorage, isoStorage, importStorage))
}

// kubeMintShadowRows is the Perl that reads `pveum acl list` as JSON and prints, for the portal user ($ARGV[0]) and
// token ($ARGV[1]), one line "--users|--tokens <id> <propagate> <path>" per path below /storage or /sdn/zones where
// that principal holds a row of its own with another role: there the row replaces the mint role the parent passes
// down (KubeMintPrivileges). propagate is 1 when any of those rows propagates, since only such a row reaches the
// paths below it, where the mint role must then follow; a mint row that propagated from a path whose other rows do
// not would itself replace what the paths below inherit. Installer stage 10 has the same rule ("Repeat the mint role").
const kubeMintShadowRows = `my ($u, $t) = @ARGV; my %p; ` +
	`for (@{ decode_json(join "", <STDIN>) }) { ` +
	`next if $_->{roleid} eq "` + KubeMintRole + `"; ` +
	`my $f = $_->{type} eq "user" && $_->{ugid} eq $u ? "--users" : $_->{type} eq "token" && $_->{ugid} eq $t ? "--tokens" : next; ` +
	`next unless $_->{path} =~ m{^/storage/[^/]+$|^/sdn/zones/.}; ` +
	`$p{"$f $_->{ugid}"}{$_->{path}} ||= $_->{propagate} ? 1 : 0 } ` +
	`for my $k (sort keys %p) { print "$k $p{$k}{$_} $_\n" for sort keys %{ $p{$k} } }`

func setupScript(tokenID string, gpu bool, importStorage string, managed []ManagedACL) (string, error) {
	m := setupTokenID.FindStringSubmatch(tokenID)
	if m == nil {
		return "", fmt.Errorf("tokenId must be user@realm!name, of letters, digits, dot, dash and underscore")
	}
	if importStorage != "" && !setupStorageID.MatchString(importStorage) {
		return "", fmt.Errorf("importStorage must be a storage id: a letter, then letters, digits, dot, dash and underscore")
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# ProxCloud: the Proxmox VE identity of the portal token.")
	line("# Paste into a root shell on one node (the cluster shares it). Safe to run again.")
	line("(")
	line("set -eu")
	line("PC_USER=%s", q(m[1]))
	line("PC_TOKEN_NAME=%s", q(m[2]))
	line("PC_TOKEN=%s", q(tokenID))
	line("")
	line("# role: created empty when missing, then the privileges are appended (a re-run adds new ones, removes none)")
	line("pveum role add %s 2>/dev/null || true", q(PlatformRole))
	line("pveum role modify %s --append 1 --privs %s", q(PlatformRole), q(strings.Join(PlatformRolePrivileges(), " ")))
	line("")
	line("# pools: OS templates, and the management VMs the token must not reach")
	line("pveum pool add %s --comment %s 2>/dev/null || true", q(TemplatePool), q("ProxCloud templates"))
	line("pveum pool add %s --comment %s 2>/dev/null || true", q(SystemPool), q("ProxCloud management (not tenant)"))
	line("")
	line("# user and privilege-separated token. The secret is printed once: store it")
	line(`pveum user add "$PC_USER" --comment %s 2>/dev/null || true`, q("ProxCloud portal"))
	line(`pveum user token add "$PC_USER" "$PC_TOKEN_NAME" --privsep 1 || echo "The token $PC_TOKEN exists already: its secret was printed when it was created."`)
	line("")
	line("# ACLs, for the user and for the token: a privilege-separated token holds the intersection of the two")
	line("grant() { # path role propagate")
	line(`  pveum acl modify "$1" --users "$PC_USER" --roles "$2" --propagate "$3"`)
	line(`  pveum acl modify "$1" --tokens "$PC_TOKEN" --roles "$2" --propagate "$3"`)
	line("}")
	grant := func(path, role string, propagate bool) {
		n := 0
		if propagate {
			n = 1
		}
		line("grant %s %s %d", q(path), q(role), n)
	}
	for _, a := range PlatformACLs {
		if !a.GPUOnly || gpu {
			grant(a.Path, a.Role, a.Propagate)
		}
	}
	if importStorage != "" {
		grant("/storage/"+importStorage, "PVEDatastoreAdmin", false)
	}
	if managed != nil {
		line("")
		line("# managed services: roles, the seed pool, the users and tokens of CAPMOX and proxmox-csi on the platform, and")
		line("# their ACLs; the portal token may then mint the tokens of tenant clusters. Both secrets are printed once: store them")
		for _, r := range ManagedRoles {
			line("pveum role add %s 2>/dev/null || true", q(r.Name))
			line("pveum role modify %s --append 1 --privs %s", q(r.Name), q(strings.Join(r.Privs, " ")))
		}
		line("pveum pool add %s --comment %s 2>/dev/null || true", q(SeedPool), q("ProxCloud seed cluster (not tenant)"))
		for _, t := range []struct{ id, comment string }{{CAPITokenID, "ProxCloud CAPI"}, {CSITokenID, "ProxCloud CSI"}} {
			user, name, _ := strings.Cut(t.id, "!")
			line("pveum user add %s --comment %s 2>/dev/null || true", q(user), q(t.comment))
			line("pveum user token add %s %s --privsep 1 || echo %s", q(user), q(name),
				q("The token "+t.id+" exists already: its secret was printed when it was created."))
		}
		line("# the only users the portal token may create and delete tokens for. Its grant sits on this group, never on")
		line("# /access/groups, which reaches every user of the cluster (root@pam too)")
		line("pveum group add %s --comment %s 2>/dev/null || true", q(KubeMintGroup), q("ProxCloud: users the portal mints cluster tokens for"))
		for _, u := range KubeMintGroupMembers {
			line("pveum user modify %s --groups %s --append 1", q(u), q(KubeMintGroup))
		}
		for _, a := range managed {
			if a.User == m[1] && a.Token == tokenID {
				grant(a.Path, a.Role, a.Propagate)
				continue
			}
			n := 0
			if a.Propagate {
				n = 1
			}
			for _, p := range []struct{ flag, id string }{{"--users", a.User}, {"--tokens", a.Token}} {
				if p.id != "" {
					line("pveum acl modify %s %s %s --roles %s --propagate %d", q(a.Path), p.flag, q(p.id), q(a.Role), n)
				}
			}
		}
		line("# a row of this user or token on a single store or zone replaces what /storage and /sdn/zones pass down there, the")
		line("# mint role too, and a tenant cluster on that store is then refused. The mint role is written next to each such row,")
		line("# for the one that holds the row (a row for the other would replace what that one inherits), propagated as the row is")
		line(`PC_SHADOW=$(pveum acl list --output-format json | perl -MJSON::PP -e %s "$PC_USER" "$PC_TOKEN")`, q(kubeMintShadowRows))
		line(`printf '%%s\n' "$PC_SHADOW" | while read -r PC_FLAG PC_ID PC_PROP PC_PATH; do`)
		line(`  [ -z "$PC_PATH" ] || pveum acl modify "$PC_PATH" "$PC_FLAG" "$PC_ID" --roles %s --propagate "$PC_PROP" </dev/null`, q(KubeMintRole))
		line("done")
		line("# a site set up before the group: the mint role sat on /access/groups and /pool and held User.Modify. These remove")
		line("# exactly this token's rows, after the grant on the group above, and change nothing on a new site")
		for _, path := range KubeMintOldPaths {
			line(`pveum acl delete %s --users "$PC_USER" --roles %s`, q(path), q(KubeMintRole))
			line(`pveum acl delete %s --tokens "$PC_TOKEN" --roles %s`, q(path), q(KubeMintRole))
		}
		line("# User.Modify leaves the role for every holder at once, so only when no other portal token still creates tokens")
		line("# through a row on /access/groups: run this script for each of them, the last run takes the privilege out")
		line(`PC_LEFT=$(pveum acl list --output-format json | perl -MJSON::PP -e %s)`,
			q(`print map { "  $_->{type} $_->{ugid}\n" } grep { $_->{path} eq "/access/groups" && $_->{roleid} eq "`+KubeMintRole+`" } @{ decode_json(join "", <STDIN>) }`))
		line(`[ -n "$PC_LEFT" ] || pveum role modify %s --privs %s`, q(KubeMintRole), q(strings.Join(ManagedRoles[2].Privs, " ")))
		line("pc_left() { # said last, when this subshell ends, below everything the script prints")
		line(`  echo %s`, q("NOT FINISHED: "+KubeMintRole+" still holds User.Modify, because these still hold that role on /access/groups:"))
		line(`  echo "$PC_LEFT"`)
		line(`  echo %s`, q("Each is another portal token, or its user, that creates cluster tokens through that row and would stop if the role lost"))
		line(`  echo %s`, q("User.Modify now. Run this script for each such token (tokenId=<its id>), or make upgrade for the token of an installer site;"))
		line(`  echo %s`, q("the last run takes User.Modify out of the role. Until then a token listed here whose user is listed too can create an API"))
		line(`  echo %s`, q("token for any user of the cluster, root@pam included. A row no portal uses: pveum acl delete /access/groups --users <user>"))
		line(`  echo %s`, q("(or --tokens <token id>, in single quotes) --roles "+KubeMintRole+", then this script again."))
		line("}")
		line(`[ -z "$PC_LEFT" ] || trap pc_left EXIT`)
		line("# what the platform tokens hold on the platform pools: the CAPI token ProxCloudCAPI on both, the CSI token nothing on the")
		line(`# seed VM (its disks on the boot store stay open to the CSI token: docs/proxmox-permissions.md, "What the seed pool does not fence")`)
		for _, id := range []string{CAPITokenID, CSITokenID} {
			user, name, _ := strings.Cut(id, "!")
			for _, pool := range []string{SystemPool, SeedPool} {
				line("pveum user token permissions %s %s --path %s", q(user), q(name), q("/pool/"+pool))
			}
		}
	}
	line("")
	line("# what the token really holds")
	line(`pveum user token permissions "$PC_USER" "$PC_TOKEN_NAME"`)
	line("# the certificate fingerprint to pin on the provider: pve-ssl.pem, or pveproxy-ssl.pem when the node has its own")
	line("pvenode cert info")
	line(")")
	return b.String(), nil
}
