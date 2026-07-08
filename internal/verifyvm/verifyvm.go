// Package verifyvm builds the Proxmox `qm`/`pvesm` command plan for
// creating and destroying a disposable "verify-mseg-tester" VM used to
// exercise mseg-tester's cloud-init + binary end-to-end on real hardware.
//
// Every setting is a Params field populated entirely from command-line
// flags (see cmd/verify-mseg-tester) -- this package carries no
// environment-specific defaults (no hardcoded host, VMID, storage name,
// bridge, or credential) since it's meant to be published alongside
// mseg-tester and used against any Proxmox host.
//
// Command construction (this package) is kept separate from command
// execution (internal/sshrun) so the plan can be built and printed
// without ever opening a network connection -- see BuildCreatePlan's
// Step.Command/Step.Action distinction and cmd/verify-mseg-tester's
// dry-run-by-default behavior.
package verifyvm

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

//go:embed user-data.yaml.tmpl
var userDataTemplate string

// NetworkConfigDisabled is uploaded as the cloud-init "network" cicustom
// snippet -- it tells cloud-init not to generate its own netplan config
// at all, so the ONLY netplan file present is the one this tool's own
// user-data write_files entry creates (/etc/netplan/90-mseg-tester.yaml).
// Without this, Proxmox's NoCloud datasource falls back to a DHCP
// network-config of its own, which would fight the trunk NIC's
// deliberately-unconfigured ethernet stanza.
//
//go:embed network-config-disabled.yaml
var NetworkConfigDisabled []byte

// Params is every setting needed to create, destroy, or check the status
// of a verify-mseg-tester VM.
type Params struct {
	// Reaching the Proxmox host.
	Host    string   // ssh target, e.g. "root@proxmox.example.com"
	SSHOpts []string // extra args passed to ssh verbatim, e.g. ["-p", "2222"]

	// Identifying the VM.
	VMID int
	Name string

	// create-only: disk/image/network placement.
	Storage    string
	Image      string // path to a cloud-init-ready disk image, ON the Proxmox host
	Bridge     string
	TrunkVLANs []string // VLAN IDs to trunk, TAGGED; empty = untagged (every VLAN passes). Does not include NativeSegment -- see net0.
	Cores      int
	MemoryMB   int
	DiskSize        string // e.g. "8G", passed to `qm resize`
	BIOS            string // "seabios" or "ovmf"
	Onboot          bool
	Start           bool
	SnippetsStorage string
	SnippetsPath    string // filesystem path to that storage's snippets dir, ON the Proxmox host

	// create-only: rendered into the cloud-init user-data (see
	// internal/bootstrap for what each of these means on the guest side).
	TrunkIface string
	// NativeSegment is OPTIONAL -- see bootstrap.Bootstrap.NativeSegment.
	// When set, the Proxmox NIC is configured with that VLAN as net0's
	// "tag=" (untagged/native) rather than lumped into "trunks=" with
	// the other, genuinely tagged segments -- see net0.
	NativeSegment    string
	UpdateSegment    string
	SoftwareRepo     string
	ConfigRepo       string // optional: private repo URL, see bootstrap.Bootstrap.ConfigRepo
	ConfigPath       string
	ConfigRef        string
	ConfigToken      string
	SSHAuthorizedKey string
	// ConfigYAML is the raw content of a plain config.yaml to write
	// directly at deploy time -- the "make it easy first" path with no
	// private repo or token at all. At least one of ConfigRepo or
	// ConfigYAML must be set; both may be, in which case ConfigYAML is
	// just the starting point until the first sync from ConfigRepo
	// overwrites it.
	ConfigYAML string
	// SoftwareRef is OPTIONAL -- the git branch/tag/commit the bootstrap
	// script's `go install` (and every later self-update,
	// internal/selfupdate) builds from. Defaults to "latest" (the newest
	// semver tag) if empty. Point it at your own branch or a commit SHA
	// to exercise this whole tool against unreleased code -- no GitHub
	// release, no build pipeline, no binary-hosting side channel needed
	// ("test without gh"): `go install` fetches straight from
	// SoftwareRepo's git history via the Go module proxy.
	SoftwareRef string

	// destroy-only.
	KeepSnippets bool
}

func (p Params) userDataSnippet() string { return p.Name + "-userdata.yaml" }
func (p Params) networkSnippet() string  { return p.Name + "-network.yaml" }

var templateFuncs = template.FuncMap{
	// indent prefixes every line of s with prefix -- used to fit a
	// verbatim config.yaml under a YAML block-scalar "content: |" at the
	// right indentation. A trailing blank line (from a trailing newline
	// in s) is dropped so it doesn't leave a stray empty content line.
	"indent": func(s, prefix string) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i, l := range lines {
			lines[i] = prefix + l
		}
		return strings.Join(lines, "\n")
	},
}

// RenderUserData executes the embedded cloud-init template against p.
func (p Params) RenderUserData() ([]byte, error) {
	tmpl, err := template.New("user-data").Funcs(templateFuncs).Parse(userDataTemplate)
	if err != nil {
		return nil, fmt.Errorf("verifyvm: parsing embedded template: %w", err)
	}
	data := struct {
		Hostname         string
		TrunkIface       string
		NativeSegment    string
		UpdateSegment    string
		SoftwareRepo     string
		SoftwareRef      string
		ConfigRepo       string
		ConfigPath       string
		ConfigRef        string
		ConfigToken      string
		SSHAuthorizedKey string
		ConfigYAML       string
	}{
		Hostname:         p.Name,
		TrunkIface:       p.TrunkIface,
		NativeSegment:    p.NativeSegment,
		UpdateSegment:    p.UpdateSegment,
		SoftwareRepo:     p.SoftwareRepo,
		SoftwareRef:      p.SoftwareRef,
		ConfigRepo:       p.ConfigRepo,
		ConfigPath:       p.ConfigPath,
		ConfigRef:        p.ConfigRef,
		ConfigToken:      p.ConfigToken,
		SSHAuthorizedKey: p.SSHAuthorizedKey,
		ConfigYAML:       p.ConfigYAML,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("verifyvm: rendering template: %w", err)
	}
	return buf.Bytes(), nil
}

// net0 builds the `qm` NIC argument. With no NativeSegment, this is
// either a fully-untagged trunk (no TrunkVLANs given -- every VLAN
// passes) or restricted to TrunkVLANs. With NativeSegment set, that one
// VLAN is instead Proxmox's "tag=" (delivered untagged/native), and only
// the OTHER, genuinely-tagged segments go into "trunks=" -- matching
// bootstrap.Bootstrap.NativeSegment/internal/netplan.IfaceName's split on
// the guest side. See ValidateCreate: NativeSegment (if set) must also
// appear in TrunkVLANs, since that's the one place the operator declares
// the full segment list to this tool.
func (p Params) net0() string {
	if p.NativeSegment == "" {
		if len(p.TrunkVLANs) == 0 {
			return fmt.Sprintf("virtio,bridge=%s", p.Bridge)
		}
		return fmt.Sprintf("virtio,bridge=%s,trunks=%s", p.Bridge, strings.Join(p.TrunkVLANs, ";"))
	}
	var tagged []string
	for _, v := range p.TrunkVLANs {
		if v != p.NativeSegment {
			tagged = append(tagged, v)
		}
	}
	if len(tagged) == 0 {
		return fmt.Sprintf("virtio,bridge=%s,tag=%s", p.Bridge, p.NativeSegment)
	}
	return fmt.Sprintf("virtio,bridge=%s,tag=%s,trunks=%s", p.Bridge, p.NativeSegment, strings.Join(tagged, ";"))
}

// ValidateCommon checks the flags every subcommand needs.
func (p Params) ValidateCommon() error {
	var missing []string
	if p.Host == "" {
		missing = append(missing, "-host")
	}
	if p.VMID <= 0 {
		missing = append(missing, "-vmid")
	}
	if p.Name == "" {
		missing = append(missing, "-name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required flags: %s", strings.Join(missing, ", "))
	}
	return nil
}

// ValidateCreate additionally checks everything `create` needs.
func (p Params) ValidateCreate() error {
	if err := p.ValidateCommon(); err != nil {
		return err
	}
	required := map[string]string{
		"-storage":        p.Storage,
		"-image":          p.Image,
		"-bridge":         p.Bridge,
		"-update-segment": p.UpdateSegment,
		"-software-repo":  p.SoftwareRepo,
	}
	var missing []string
	for flag, v := range required {
		if v == "" {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing required flags: %s", strings.Join(missing, ", "))
	}
	// config.yaml needs to come from somewhere: either a plain file
	// provided directly (-config-file, no repo/token needed -- the
	// "make it easy first" path) or a private repo to fetch it from at
	// runtime (-config-repo, optionally with -config-token/-token-file
	// if that repo is private). Both may be set; neither is invalid.
	if p.ConfigRepo == "" && p.ConfigYAML == "" {
		return fmt.Errorf("provide config.yaml via -config-file, or point at a repo to fetch it from via -config-repo (or both)")
	}
	if p.BIOS != "seabios" && p.BIOS != "ovmf" {
		return fmt.Errorf("-bios must be \"seabios\" or \"ovmf\", got %q", p.BIOS)
	}
	if p.NativeSegment != "" && len(p.TrunkVLANs) > 0 {
		found := false
		for _, v := range p.TrunkVLANs {
			if v == p.NativeSegment {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("-native-segment %q must also be listed in -trunk-vlans", p.NativeSegment)
		}
	}
	return nil
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func boolToInt(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Runner executes one remote command (optionally piping stdin to it) and
// returns its combined output -- implemented by internal/sshrun. Kept as
// an interface here so this package's plan-building logic never imports
// os/exec directly and stays trivially testable.
type Runner interface {
	Run(command string, stdin []byte) (output string, err error)
}

// Step is one unit of work against the Proxmox host: either a fixed
// remote command (Command, optionally with Stdin piped to it -- used for
// uploading rendered files via `cat > path`), or an Action for steps that
// need to inspect live state first (e.g. "only touch this storage's
// content types if snippets isn't already enabled"). Dry-run mode (see
// cmd/verify-mseg-tester) prints every Step's Description without
// invoking Command or Action -- no Step construction here ever opens a
// connection itself.
type Step struct {
	Description string
	Command     string
	Stdin       []byte
	Action      func(Runner) error
}

// BuildCreatePlan returns every step needed to bring the VM up, in order.
func (p Params) BuildCreatePlan() ([]Step, error) {
	userData, err := p.RenderUserData()
	if err != nil {
		return nil, err
	}

	var steps []Step

	steps = append(steps, Step{
		Description: fmt.Sprintf("ensure storage %q has the snippets content type enabled (only if not already)", p.SnippetsStorage),
		Action:      p.ensureSnippetsEnabled,
	})

	steps = append(steps, Step{
		Description: fmt.Sprintf("upload rendered cloud-init user-data to %s/%s", p.SnippetsPath, p.userDataSnippet()),
		Command:     fmt.Sprintf("cat > %s", shQuote(p.SnippetsPath+"/"+p.userDataSnippet())),
		Stdin:       userData,
	})

	steps = append(steps, Step{
		Description: fmt.Sprintf("upload network-config (disables cloud-init's own netplan generation) to %s/%s", p.SnippetsPath, p.networkSnippet()),
		Command:     fmt.Sprintf("cat > %s", shQuote(p.SnippetsPath+"/"+p.networkSnippet())),
		Stdin:       NetworkConfigDisabled,
	})

	createArgs := []string{
		"qm", "create", strconv.Itoa(p.VMID),
		"--name", shQuote(p.Name),
		"--ostype", "l26",
		"--cores", strconv.Itoa(p.Cores),
		"--memory", strconv.Itoa(p.MemoryMB),
		"--numa", "0",
		"--scsihw", "virtio-scsi-single",
		"--net0", shQuote(p.net0()),
		"--onboot", boolToInt(p.Onboot),
		"--serial0", "socket",
		"--vga", "serial0",
		"--citype", "nocloud",
		"--ciuser", "ubuntu",
		"--cicustom", shQuote(fmt.Sprintf("user=%s:snippets/%s,network=%s:snippets/%s",
			p.SnippetsStorage, p.userDataSnippet(), p.SnippetsStorage, p.networkSnippet())),
	}
	if p.BIOS == "ovmf" {
		createArgs = append(createArgs, "--bios", "ovmf", "--machine", "q35",
			"--efidisk0", fmt.Sprintf("%s:0,efitype=4m,pre-enrolled-keys=0", p.Storage))
	}
	steps = append(steps, Step{
		Description: fmt.Sprintf("create VM %d (%s)", p.VMID, p.Name),
		Command:     strings.Join(createArgs, " "),
	})

	steps = append(steps, Step{
		Description: fmt.Sprintf("import %s onto storage %q", p.Image, p.Storage),
		Command:     fmt.Sprintf("qm importdisk %d %s %s", p.VMID, shQuote(p.Image), shQuote(p.Storage)),
	})

	steps = append(steps, Step{
		Description: "attach the imported disk as scsi0 (whatever `qm importdisk` just attached as \"unused\")",
		Action:      p.attachImportedDisk,
	})

	steps = append(steps,
		Step{
			Description: "attach the cloud-init drive",
			Command:     fmt.Sprintf("qm set %d --ide2 %s:cloudinit", p.VMID, shQuote(p.Storage)),
		},
		Step{
			Description: "set boot order to scsi0",
			Command:     fmt.Sprintf("qm set %d --boot order=scsi0", p.VMID),
		},
		Step{
			Description: fmt.Sprintf("resize scsi0 to %s", p.DiskSize),
			Command:     fmt.Sprintf("qm resize %d scsi0 %s", p.VMID, shQuote(p.DiskSize)),
		},
	)

	if p.Start {
		steps = append(steps, Step{
			Description: fmt.Sprintf("start VM %d", p.VMID),
			Command:     fmt.Sprintf("qm start %d", p.VMID),
		})
	}

	return steps, nil
}

// ensureSnippetsEnabled queries the storage's current `content` types and
// only runs `pvesm set` if "snippets" isn't already among them, so this
// never clobbers content types the storage already had configured for
// something else (e.g. iso/vztmpl/backup).
func (p Params) ensureSnippetsEnabled(r Runner) error {
	out, err := r.Run(fmt.Sprintf("pvesh get /storage/%s --output-format=json", shQuote(p.SnippetsStorage)), nil)
	if err != nil {
		return fmt.Errorf("querying storage %q: %w", p.SnippetsStorage, err)
	}
	var cfg struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return fmt.Errorf("parsing storage %q config: %w", p.SnippetsStorage, err)
	}
	for _, t := range splitNonEmpty(cfg.Content, ",") {
		if t == "snippets" {
			return nil // already enabled, nothing to do
		}
	}
	types := append(splitNonEmpty(cfg.Content, ","), "snippets")
	if _, err := r.Run(fmt.Sprintf("pvesm set %s --content %s", shQuote(p.SnippetsStorage), shQuote(strings.Join(types, ","))), nil); err != nil {
		return fmt.Errorf("enabling snippets on storage %q: %w", p.SnippetsStorage, err)
	}
	return nil
}

// attachImportedDisk finds the disk `qm importdisk` just attached as an
// "unused" volume and promotes it to scsi0 -- avoids hardcoding a disk
// index, which depends on how many disks (e.g. an efidisk0) already
// existed on the VM before the import.
func (p Params) attachImportedDisk(r Runner) error {
	out, err := r.Run(fmt.Sprintf("qm config %d", p.VMID), nil)
	if err != nil {
		return fmt.Errorf("reading VM %d config: %w", p.VMID, err)
	}
	var volid string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "unused") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				volid = strings.SplitN(strings.TrimSpace(parts[1]), ",", 2)[0]
				break
			}
		}
	}
	if volid == "" {
		return fmt.Errorf("no \"unused\" disk found on VM %d after importdisk -- expected one from `qm importdisk`", p.VMID)
	}
	if _, err := r.Run(fmt.Sprintf("qm set %d --scsi0 %s,iothread=1,discard=on", p.VMID, shQuote(volid)), nil); err != nil {
		return fmt.Errorf("attaching %s as scsi0: %w", volid, err)
	}
	return nil
}

// BuildDestroyPlan stops and purges the VM, and removes the snippet files
// this tool created for it (unless KeepSnippets).
func (p Params) BuildDestroyPlan() []Step {
	steps := []Step{
		{
			Description: fmt.Sprintf("stop VM %d if running", p.VMID),
			Command:     fmt.Sprintf("qm stop %d --timeout 30 || true", p.VMID),
		},
		{
			Description: fmt.Sprintf("destroy VM %d (--purge)", p.VMID),
			Command:     fmt.Sprintf("qm destroy %d --purge", p.VMID),
		},
	}
	if !p.KeepSnippets {
		steps = append(steps, Step{
			Description: "remove the cloud-init snippet files `create` uploaded",
			Command: fmt.Sprintf("rm -f %s %s",
				shQuote(p.SnippetsPath+"/"+p.userDataSnippet()),
				shQuote(p.SnippetsPath+"/"+p.networkSnippet())),
		})
	}
	return steps
}

// BuildStatusPlan is read-only -- cmd/verify-mseg-tester runs it
// unconditionally (no --yes gate needed for read-only commands).
func (p Params) BuildStatusPlan() []Step {
	return []Step{
		{Description: fmt.Sprintf("qm status %d", p.VMID), Command: fmt.Sprintf("qm status %d", p.VMID)},
		{Description: fmt.Sprintf("qm config %d", p.VMID), Command: fmt.Sprintf("qm config %d", p.VMID)},
	}
}
