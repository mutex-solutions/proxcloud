package pve

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The script printed in docs/proxmox-permissions.md is what the API generates: an operator reading the page and one
// using the console run the same commands.
func TestPlatformSetupScriptMatchesDocs(t *testing.T) {
	doc, err := os.ReadFile("../../../docs/proxmox-permissions.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "<!-- generated: GET /api/v1/admin/setup/proxmox-script?gpu=true -->\n```sh\n")
	block, _, closed := strings.Cut(rest, "```\n")
	if !ok || !closed {
		t.Fatal("docs/proxmox-permissions.md has no generated script block")
	}
	got, err := PlatformSetupScript(DefaultPlatformTokenID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != block {
		t.Fatalf("docs/proxmox-permissions.md differs from the generated script. Generated:\n%s", got)
	}
}

// The options change exactly the lines they are about.
func TestPlatformSetupScriptOptions(t *testing.T) {
	lines := func(tokenID string, gpu bool, importStorage string) []string {
		t.Helper()
		s, err := PlatformSetupScript(tokenID, gpu, importStorage)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(s, "\n")
	}
	base := lines(DefaultPlatformTokenID, true, "")
	tests := []struct {
		name          string
		tokenID       string
		gpu           bool
		importStorage string
		gone, added   []string
	}{
		{"defaults", DefaultPlatformTokenID, true, "", nil, nil},
		{"no GPUs", DefaultPlatformTokenID, false, "", []string{"grant '/mapping/pci' 'ProxCloudPlatform' 1"}, nil},
		{"import store", DefaultPlatformTokenID, true, "nas-templates", nil, []string{"grant '/storage/nas-templates' 'PVEDatastoreAdmin' 0"}},
		{"another token", "portal.svc@pve!cloud_1", true, "",
			[]string{"PC_USER='proxcloud@pve'", "PC_TOKEN_NAME='platform'", "PC_TOKEN='proxcloud@pve!platform'"},
			[]string{"PC_USER='portal.svc@pve'", "PC_TOKEN_NAME='cloud_1'", "PC_TOKEN='portal.svc@pve!cloud_1'"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lines(tt.tokenID, tt.gpu, tt.importStorage)
			var gone, added []string
			for _, l := range base {
				if !slices.Contains(got, l) {
					gone = append(gone, l)
				}
			}
			for _, l := range got {
				if !slices.Contains(base, l) {
					added = append(added, l)
				}
			}
			if !slices.Equal(gone, tt.gone) || !slices.Equal(added, tt.added) {
				t.Fatalf("gone %q, added %q; want gone %q, added %q", gone, added, tt.gone, tt.added)
			}
		})
	}
	// The script is pasted into an interactive root shell, where a `!` outside single quotes is history expansion
	// and a failing `set -e` outside a subshell ends the session. A re-run must not fail on what exists.
	script := strings.Join(base, "\n")
	for _, l := range base {
		if strings.Contains(l, "!") && l != "PC_TOKEN='proxcloud@pve!platform'" {
			t.Errorf("a ! outside single quotes: %s", l)
		}
	}
	for _, want := range []string{"(\nset -eu\n", "pveum role modify 'ProxCloudPlatform' --append 1 --privs '", "pveum role add 'ProxCloudPlatform' 2>/dev/null || true\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if !strings.HasSuffix(script, "pveum user token permissions \"$PC_USER\" \"$PC_TOKEN_NAME\"\n"+
		"# the certificate fingerprint to pin on the provider: pve-ssl.pem, or pveproxy-ssl.pem when the node has its own\npvenode cert info\n)\n") {
		t.Errorf("script does not end with the permissions and the fingerprint:\n%s", script)
	}
}

// A root shell runs the answer: nothing but a plain id gets into it.
func TestPlatformSetupScriptRejectsInjection(t *testing.T) {
	for _, tokenID := range []string{
		"", "proxcloud@pve", "proxcloud!platform", "@pve!platform", "proxcloud@!platform", "proxcloud@pve!",
		"proxcloud@pve!platform; rm -rf /", "proxcloud@pve!platform'; id; '", "$(id)@pve!platform", "`id`@pve!platform",
		"proxcloud@pve!plat form", "proxcloud@pve!platform\nid", "proxcloud@pve!platform\n", "proxcloud@pve!platform\\", "proxcloud@pve!$HOME",
		"proxcloud@pve!platform&&id", "proxcloud@pve!platform|id", "proxcloud@pve!-x", "-x@pve!platform",
		"proxcloud@pve!platform!again", "proxcloud@pve!plätform", "proxcloud@pve!" + strings.Repeat("a", 65),
	} {
		if s, err := PlatformSetupScript(tokenID, true, ""); err == nil {
			t.Errorf("token id %q accepted:\n%s", tokenID, s)
		}
	}
	for _, store := range []string{"nas; reboot", "nas'", "$(id)", "nas templates", "../etc", "-nas", "nas\nid", "nas/sub", "nas`id`"} {
		if s, err := PlatformSetupScript(DefaultPlatformTokenID, true, store); err == nil {
			t.Errorf("import storage %q accepted:\n%s", store, s)
		}
	}
}

// The role and the ACLs the portal knows are the ones the installer writes (stage 10).
func TestPlatformIdentityMatchesInstaller(t *testing.T) {
	raw, err := os.ReadFile("../../../installer/playbooks/10-pve-identity.yml")
	if err != nil {
		t.Fatal(err)
	}
	stage := string(raw)
	vmConfig := regexp.MustCompile(`(?m)^\s+vm_config: "([^"]+)"$`).FindStringSubmatch(stage)
	role := regexp.MustCompile(`(?m)^\s+ProxCloudPlatform: "([^"]+)"$`).FindStringSubmatch(stage)
	if vmConfig == nil || role == nil {
		t.Fatal("stage 10 has no vm_config or ProxCloudPlatform privilege list")
	}
	installer := strings.Fields(strings.ReplaceAll(role[1], "{{ vm_config }}", vmConfig[1]))
	portal := PlatformRolePrivileges()
	slices.Sort(installer)
	slices.Sort(portal)
	if !slices.Equal(installer, portal) {
		t.Errorf("ProxCloudPlatform privileges differ:\ninstaller %q\nportal    %q", installer, portal)
	}

	var installerACLs []PlatformACL
	rows := regexp.MustCompile(`(?m)^\s+- \{path: "?([^,"]+)"?, role: (\w+), propagate: ([01]), user: proxcloud@pve, token: proxcloud@pve!platform\}$`)
	base, _, _ := strings.Cut(stage, "\n    nas_acls:") // the rows every install gets; what follows depends on its mode
	for _, m := range rows.FindAllStringSubmatch(base, -1) {
		path := strings.ReplaceAll(m[1], "{{ pc.pve.system_pool }}", SystemPool)
		installerACLs = append(installerACLs, PlatformACL{Path: path, Role: m[2], Propagate: m[3] == "1", GPUOnly: path == "/mapping/pci"})
	}
	if !slices.Equal(installerACLs, PlatformACLs) {
		t.Errorf("platform ACLs differ:\ninstaller %+v\nportal    %+v", installerACLs, PlatformACLs)
	}
	// Every path the connection test reads privileges on is covered by an ACL, on itself or a parent that propagates.
	for _, chk := range PlatformPrivileges {
		covered := slices.ContainsFunc(PlatformACLs, func(a PlatformACL) bool {
			return a.Path == chk.Path || (a.Propagate && strings.HasPrefix(chk.Path, a.Path+"/"))
		})
		if !covered {
			t.Errorf("no ACL gives the token anything on %s", chk.Path)
		}
	}
}
