package pve

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// The managed-services script is the portal token's script with one block before its last checks, and that block is
// the one printed in docs/proxmox-permissions.md.
func TestManagedSetupScriptMatchesDocs(t *testing.T) {
	doc, err := os.ReadFile("../../../docs/proxmox-permissions.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "<!-- generated: the lines GET /api/v1/admin/setup/proxmox-script?gpu=true&managedServices=true&bootStorage=local-lvm&isoStorage=local adds -->\n```sh\n")
	block, _, closed := strings.Cut(rest, "```\n")
	if !ok || !closed {
		t.Fatal("docs/proxmox-permissions.md has no generated managed-services block")
	}
	base, err := PlatformSetupScript(DefaultPlatformTokenID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ManagedSetupScript(DefaultPlatformTokenID, true, "", "local-lvm", "local")
	if err != nil {
		t.Fatal(err)
	}
	at := strings.Index(base, "\n# what the token really holds\n")
	if want := base[:at] + "\n" + block + base[at:]; got != want {
		t.Fatalf("the managed-services script is not the portal token's script plus the block of docs/proxmox-permissions.md. Generated:\n%s", got)
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
	managed, err := ManagedSetupScript(DefaultPlatformTokenID, true, "", "local-lvm", "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range append(base, strings.Split(managed, "\n")...) {
		if bangOutsideSingleQuotes(l) {
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

// With managed services the import store keeps the mint role: its PVEDatastoreAdmin row replaces what /storage passes
// down, and a tenant cluster's ISO or boot store may be that store (stage 10 repeats it in nas_mint_acls).
func TestManagedSetupScriptImportStorage(t *testing.T) {
	base, err := ManagedSetupScript(DefaultPlatformTokenID, true, "", "local-lvm", "local")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ManagedSetupScript(DefaultPlatformTokenID, true, "local", "local-lvm", "local")
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, l := range strings.Split(got, "\n") {
		if !slices.Contains(strings.Split(base, "\n"), l) {
			added = append(added, l)
		}
	}
	if want := []string{"grant '/storage/local' 'PVEDatastoreAdmin' 0", "grant '/storage/local' 'ProxCloudKubeMint' 0"}; !slices.Equal(added, want) {
		t.Errorf("importStorage added %q, want %q", added, want)
	}
}

// The portal's right to create tokens sits on the group of its two service users, and a site an earlier script set
// up loses exactly the rows and the privilege that script wrote, after the new grant so minting never stops.
func TestManagedSetupScriptNarrowsTheMintGrant(t *testing.T) {
	script, err := ManagedSetupScript("portal@pve!site-a", false, "", "local-lvm", "local")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pveum group add 'proxcloud-kube' --comment 'ProxCloud: users the portal mints cluster tokens for' 2>/dev/null || true",
		"pveum user modify 'capmox@pve' --groups 'proxcloud-kube' --append 1",
		"pveum user modify 'csi@pve' --groups 'proxcloud-kube' --append 1",
		"grant '/access/groups/proxcloud-kube' 'ProxCloudKubeToken' 0",
		`pveum acl delete '/access/groups' --users "$PC_USER" --roles 'ProxCloudKubeMint'`,
		`pveum acl delete '/access/groups' --tokens "$PC_TOKEN" --roles 'ProxCloudKubeMint'`,
		"pveum role modify 'ProxCloudKubeMint' --privs 'Permissions.Modify'",
	}
	at := 0
	for _, l := range strings.Split(script, "\n") {
		if at < len(want) && l == want[at] {
			at++
		}
		if strings.HasPrefix(l, "grant '/access/groups' ") || strings.HasPrefix(l, "grant '/access' ") || strings.Contains(l, "User.Modify Permissions.Modify") {
			t.Errorf("the script still grants on all of /access/groups: %s", l)
		}
		if strings.Contains(l, "pveum user modify") && !slices.Contains(want, l) || strings.Contains(l, "acl delete") && !slices.Contains(want, l) {
			t.Errorf("an unexpected membership or removal: %s", l)
		}
	}
	if at != len(want) {
		t.Errorf("the script lacks, in this order, %q:\n%s", want[at], script)
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
		for _, args := range [][3]string{{store, "local-lvm", "local"}, {"", store, "local"}, {"", "local-lvm", store}} {
			if s, err := ManagedSetupScript(DefaultPlatformTokenID, true, args[0], args[1], args[2]); err == nil {
				t.Errorf("managed script with storages %q accepted:\n%s", args, s)
			}
		}
	}
	if s, err := ManagedSetupScript(DefaultPlatformTokenID, true, "", "", "local"); err == nil {
		t.Errorf("managed script without a boot store accepted:\n%s", s)
	}
	if s, err := ManagedSetupScript("proxcloud@pve!platform'; id; '", true, "", "local-lvm", "local"); err == nil {
		t.Errorf("managed script with a bad token id accepted:\n%s", s)
	}
}

// bangOutsideSingleQuotes: an interactive shell expands history on a `!` anywhere but inside single quotes, comments
// and double quotes included.
func bangOutsideSingleQuotes(line string) bool {
	quoted := false
	for _, r := range line {
		switch {
		case r == '\'':
			quoted = !quoted
		case r == '!' && !quoted:
			return true
		}
	}
	return false
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

// The managed-services rows and roles are stage 10's split mode: the same roles once {{ vm_config }} is expanded, and
// the same rows (split_acls, then nas_mint_acls for the import store) once its Jinja paths are expanded and the CSI
// gateway's token (no listener on Compose) is left out. The proxcloud-seed rows are the one difference.
func TestManagedIdentityMatchesInstaller(t *testing.T) {
	raw, err := os.ReadFile("../../../installer/playbooks/10-pve-identity.yml")
	if err != nil {
		t.Fatal(err)
	}
	var plays []struct {
		Vars struct {
			VMConfig   string            `yaml:"vm_config"`
			Roles      map[string]string `yaml:"roles"`
			SplitRoles map[string]string `yaml:"split_roles"`
			SplitACLs  []struct {
				Path, Role, User, Token string
				Propagate               int
			} `yaml:"split_acls"`
			NASMintACLs    string   `yaml:"nas_mint_acls"`
			MintGroup      string   `yaml:"mint_group"`
			MintGroupUsers []string `yaml:"mint_group_users"`
		} `yaml:"vars"`
	}
	if err := yaml.Unmarshal(raw, &plays); err != nil || len(plays) != 1 {
		t.Fatalf("stage 10: %v, %d plays", err, len(plays))
	}
	v := plays[0].Vars
	for _, r := range ManagedRoles {
		privs, ok := v.Roles[r.Name]
		if !ok {
			privs, ok = v.SplitRoles[r.Name]
		}
		installer := strings.Fields(strings.ReplaceAll(privs, "{{ vm_config }}", v.VMConfig))
		portal := slices.Clone(r.Privs)
		slices.Sort(installer)
		slices.Sort(portal)
		if !ok || !slices.Equal(installer, portal) {
			t.Errorf("%s privileges differ:\ninstaller %q\nportal    %q", r.Name, installer, portal)
		}
	}

	if v.MintGroup != KubeMintGroup || !slices.Equal(v.MintGroupUsers, KubeMintGroupMembers) {
		t.Errorf("stage 10 mints for %q %v, the portal for %q %v", v.MintGroup, v.MintGroupUsers, KubeMintGroup, KubeMintGroupMembers)
	}

	const boot, iso, imp = "boot-store", "iso-store", "import-store"
	jinja := strings.NewReplacer("{{ pc.services.pool }}", SystemPool, "{{ pc.services.storage }}", boot, "{{ pc.services.iso_storage }}", iso)
	var installer []ManagedACL
	for _, a := range v.SplitACLs {
		if a.Token == "csi@pve!gateway" {
			continue
		}
		row := ManagedACL{Path: jinja.Replace(a.Path), Role: a.Role, Propagate: a.Propagate == 1, User: a.User, Token: a.Token}
		if strings.Contains(row.Path, "{{") {
			t.Fatalf("stage 10 row with an unexpanded path: %+v", row)
		}
		installer = append(installer, row)
	}
	// nas_mint_acls is a Jinja list of one row on the NAS template store, stage 10's import store.
	mint := regexp.MustCompile(`^\{\{ \[\{'path': '/storage/' ~ pc\.nas\.names\.templates, 'role': '(\w+)', 'propagate': ([01]), 'user': '([^']*)', 'token': '([^']*)'\}\] if nas_acls \| length > 0 else \[\] \}\}$`).FindStringSubmatch(v.NASMintACLs)
	if mint == nil {
		t.Fatalf("stage 10 nas_mint_acls is not one row on the import store: %q", v.NASMintACLs)
	}
	installer = append(installer, ManagedACL{"/storage/" + imp, mint[1], mint[2] == "1", mint[3], mint[4]})
	var portal, seed []ManagedACL
	for _, a := range ManagedACLs(DefaultPlatformTokenID, boot, iso, imp) {
		if a.Path == "/pool/"+SeedPool {
			seed = append(seed, a)
		} else {
			portal = append(portal, a)
		}
	}
	if !slices.Equal(installer, portal) {
		t.Errorf("managed-services rows differ from stage 10's split_acls and nas_mint_acls:\ninstaller %+v\nportal    %+v", installer, portal)
	}
	wantSeed := []ManagedACL{
		{"/pool/" + SeedPool, roleCAPI, true, capiUser, CAPITokenID},
		{"/pool/" + SeedPool, "NoAccess", true, csiUser, ""},
		{"/pool/" + SeedPool, "NoAccess", true, "proxcloud@pve", DefaultPlatformTokenID},
	}
	if !slices.Equal(seed, wantSeed) {
		t.Errorf("proxcloud-seed rows = %+v, want %+v", seed, wantSeed)
	}
}
