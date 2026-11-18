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
	return setupScript(tokenID, gpu, importStorage, nil, nil)
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
	return setupScript(tokenID, gpu, importStorage, ManagedACLs(tokenID, bootStorage, isoStorage, importStorage), nil)
}

// Enrollment makes the managed-services script the root command of one installation (EnrollScript).
type Enrollment struct {
	InstallationID string
	// HaveCAPI, HaveCSI: the portal holds that token's secret, so the token on the cluster is this installation's
	// whatever its comment says (a site set up before tokens were marked).
	HaveCAPI, HaveCSI bool
}

// EnrollPrefix starts the line the root command prints and the operator pastes into the console.
const EnrollPrefix = "PC-ENROLL:"

var enrollInstallationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// EnrollScript is ManagedSetupScript as the one root action of installation e, with three differences:
//   - it marks the tokens capmox@pve!capi and csi@pve!csi as the installation's (their comment), and stops before it
//     changes anything when one exists that is neither marked so nor known to the portal (e.HaveCAPI, e.HaveCSI): a
//     fixed token id that something else may use. It never deletes or changes such a token; it prints the command the
//     operator may run after checking;
//   - it prints no token secret. The secrets of the tokens it made go into one line, EnrollPrefix plus base64 of
//     "1:<installation>:<unix time>:<capi secret>:<csi secret>" (a token that was kept has an empty field), printed
//     last, also when a later step fails;
//   - a token it kept whose secret the portal does not hold is named with the command that removes it: Proxmox shows
//     a secret once.
//
// Nothing of this was run on a Proxmox node: that pveum user token add takes --output-format json and --comment, and
// that pveum user token list prints the comment, is read from the pveum manual.
func EnrollScript(tokenID string, gpu bool, importStorage, bootStorage, isoStorage string, e Enrollment) (string, error) {
	if !enrollInstallationID.MatchString(e.InstallationID) {
		return "", fmt.Errorf("installation must be an installation id")
	}
	for _, s := range [][2]string{{"bootStorage", bootStorage}, {"isoStorage", isoStorage}} {
		if !setupStorageID.MatchString(s[1]) {
			return "", fmt.Errorf("%s must be a storage id: a letter, then letters, digits, dot, dash and underscore", s[0])
		}
	}
	return setupScript(tokenID, gpu, importStorage, ManagedACLs(tokenID, bootStorage, isoStorage, importStorage), &e)
}

// tokenState is the Perl that reads `pveum user token list <user>` as JSON and prints what the token $ARGV[0] is:
// absent, ours (its comment is $ARGV[1]) or foreign. No list (the user does not exist) is absent.
const tokenState = `my ($n, $m) = @ARGV; my $l = eval { decode_json(join "", <STDIN>) }; ` +
	`for (@{ ref $l eq "ARRAY" ? $l : [] }) { next unless $_->{tokenid} eq $n; ` +
	`print((defined $_->{comment} && $_->{comment} eq $m) ? "ours" : "foreign"); exit } print "absent"`

// kubeMintShadowRows is the Perl that reads `pveum acl list` as JSON and prints, for the portal user ($ARGV[0]) and
// token ($ARGV[1]), one line "--users|--tokens <id> <propagate> <path>" per store (/storage/<store>), zone
// (/sdn/zones/<zone>) and vnet (/sdn/zones/<zone>/<vnet>) where that principal holds a row of its own with another
// role: there the row replaces the mint role the parent passes down (KubeMintPrivileges). Those are the paths a mint
// writes or their parents (MintClusterTokens); a bridge of localnetwork (/sdn/zones/localnetwork/<bridge>) and anything
// deeper get no row from a mint, so none here: the role is Permissions.Modify. propagate is 1 when any of those rows propagates, since only such a row reaches the
// paths below it, where the mint role must then follow; a mint row that propagated from a path whose other rows do
// not would itself replace what the paths below inherit. Installer stage 10 has the same rule ("Repeat the mint role").
const kubeMintShadowRows = `my ($u, $t) = @ARGV; my %p; ` +
	`for (@{ decode_json(join "", <STDIN>) }) { ` +
	`next if $_->{roleid} eq "` + KubeMintRole + `"; ` +
	`my $f = $_->{type} eq "user" && $_->{ugid} eq $u ? "--users" : $_->{type} eq "token" && $_->{ugid} eq $t ? "--tokens" : next; ` +
	`next unless $_->{path} =~ m{^/storage/[^/]+$|^/sdn/zones/[^/]+$} || ($_->{path} =~ m{^/sdn/zones/([^/]+)/[^/]+$} && $1 ne "localnetwork"); ` +
	`$p{"$f $_->{ugid}"}{$_->{path}} ||= $_->{propagate} ? 1 : 0 } ` +
	`for my $k (sort keys %p) { print "$k $p{$k}{$_} $_\n" for sort keys %{ $p{$k} } }`

func setupScript(tokenID string, gpu bool, importStorage string, managed []ManagedACL, enroll *Enrollment) (string, error) {
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
	// The platform tokens, in the order of the enrollment line's fields.
	platformTokens := []struct {
		id, comment, v string
		have           bool
	}{{CAPITokenID, "ProxCloud CAPI", "PC_CAPI", enroll != nil && enroll.HaveCAPI}, {CSITokenID, "ProxCloud CSI", "PC_CSI", enroll != nil && enroll.HaveCSI}}
	if enroll != nil {
		line("# the two platform tokens have fixed ids. One that exists and was not made for this installation may be in use by")
		line("# something else: this command never deletes or changes it, and stops here, before it changes anything")
		line("PC_INSTALL=%s", q(enroll.InstallationID))
		line("PC_MARK=%s", q("proxcloud:"+enroll.InstallationID))
		line("pc_state() { # user token-name: absent, ours (the comment is PC_MARK) or foreign")
		line(`  pveum user token list "$1" --output-format json 2>/dev/null | perl -MJSON::PP -e %s "$2" "$PC_MARK"`, q(tokenState))
		line("}")
		line("PC_STOP=''")
		for _, t := range platformTokens {
			user, name, _ := strings.Cut(t.id, "!")
			line(`%s=$(pc_state %s %s)`, t.v, q(user), q(name))
			if t.have {
				continue // the portal holds its secret: it is this installation's, and is marked below
			}
			line(`if [ "$%s" = foreign ]; then`, t.v)
			line("  echo %s", q("STOP: the token "+t.id+" exists on this cluster and was not made for this ProxCloud installation ("+enroll.InstallationID+")."))
			line("  echo %s", q("Something else may use it, so this command does not touch it. Look at what it may do (pveum acl list | grep "+user+")"))
			line("  echo %s", q("and at who uses it. When nothing does, remove it and run this command again:"))
			line("  echo %s", q("  pveum user token remove "+user+" "+name))
			line("  PC_STOP=1")
			line("fi")
		}
		line(`if [ -n "$PC_STOP" ]; then`)
		line("  echo %s", q("Nothing was changed."))
		line("  exit 1")
		line("fi")
		line("")
	}
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
		if enroll == nil {
			line("# their ACLs; the portal token may then mint the tokens of tenant clusters. Both secrets are printed once: store them")
		} else {
			line("# their ACLs; the portal token may then mint the tokens of tenant clusters. No secret is printed: the secrets of the")
			line("# tokens made here go into the one PC-ENROLL line at the end")
		}
		for _, r := range ManagedRoles {
			line("pveum role add %s 2>/dev/null || true", q(r.Name))
			line("pveum role modify %s --append 1 --privs %s", q(r.Name), q(strings.Join(r.Privs, " ")))
		}
		line("pveum pool add %s --comment %s 2>/dev/null || true", q(SeedPool), q("ProxCloud seed cluster (not tenant)"))
		if enroll != nil {
			line("pc_token() { # user token-name state: prints the secret of a token made now; a token of an earlier setup is marked")
			line(`  case "$3" in`)
			line(`  absent) pveum user token add "$1" "$2" --privsep 1 --comment "$PC_MARK" --output-format json | perl -MJSON::PP -e %s ;;`,
				q(`print decode_json(join "", <STDIN>)->{value}`))
			line(`  foreign) pveum user token modify "$1" "$2" --comment "$PC_MARK" >/dev/null ;;`)
			line("  esac")
			line("}")
		}
		if enroll != nil {
			line("PC_CAPI_SECRET=''")
			line("PC_CSI_SECRET=''")
			line("pc_enroll() { # said last, when this subshell ends, also when a step fails after a token was made: a secret is shown once")
			line(`  if [ -n "$PC_CAPI_SECRET$PC_CSI_SECRET" ]; then`)
			line("    echo")
			line("    echo %s", q("Paste this one line into step 2 of the Managed services page. It carries the new token secrets: treat it as a password."))
			line(`    printf '%s%%s\n' "$(printf '1:%%s:%%s:%%s:%%s' "$PC_INSTALL" "$(date +%%s)" "$PC_CAPI_SECRET" "$PC_CSI_SECRET" | base64 -w0)"`, EnrollPrefix)
			line("  else")
			line("    echo %s", q("No token was made, so there is no line to paste."))
			line("  fi")
			for _, t := range platformTokens {
				if t.have {
					continue
				}
				user, name, _ := strings.Cut(t.id, "!")
				line(`  if [ "$%s" = ours ]; then`, t.v)
				line("    echo %s", q("The token "+t.id+" was made for this installation by an earlier run, and ProxCloud does not hold its secret."))
				line("    echo %s", q("Proxmox shows a secret once. Remove the token and run this command again:"))
				line("    echo %s", q("  pveum user token remove "+user+" "+name))
				line("  fi")
			}
			line("}")
			line("trap pc_enroll EXIT")
		}
		for _, t := range platformTokens {
			user, name, _ := strings.Cut(t.id, "!")
			line("pveum user add %s --comment %s 2>/dev/null || true", q(user), q(t.comment))
			if enroll != nil {
				line(`%[1]s_SECRET=$(pc_token %[2]s %[3]s "$%[1]s")`, t.v, q(user), q(name))
				continue
			}
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
		line("# a row of this user or token on a single store, zone or vnet replaces what /storage and /sdn/zones pass down there, the")
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
		if enroll == nil {
			line(`[ -z "$PC_LEFT" ] || trap pc_left EXIT`)
		} else {
			line(`[ -z "$PC_LEFT" ] || trap 'pc_left; pc_enroll' EXIT`)
		}
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
