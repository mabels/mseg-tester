package verifyvm

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderUserDataInstallsWifiAndFirmwarePackages(t *testing.T) {
	// Regression: this template used to be missing wpasupplicant/
	// linux-firmware/package_upgrade entirely (only cloud-init/user-data.yaml,
	// the OTHER "production" template, had them) -- confirmed live on a
	// verify-mseg-tester-created VM: mt7921e's firmware load failed
	// ("hardware init failed") and mseg-tester's own service never even
	// got a chance to deploy. Both templates must stay in parity.
	p := Params{Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129", SoftwareRepo: "mabels/mseg-tester"}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "package_upgrade: true") {
		t.Errorf("expected package_upgrade: true, got:\n%s", s)
	}
	if !strings.Contains(s, "- wpasupplicant") {
		t.Errorf("expected wpasupplicant in packages, got:\n%s", s)
	}
	if !strings.Contains(s, "- linux-firmware") {
		t.Errorf("expected linux-firmware in packages, got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataNoConsolePasswordLocksAccount(t *testing.T) {
	p := Params{Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129", SoftwareRepo: "mabels/mseg-tester"}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "lock_passwd: true") {
		t.Errorf("expected lock_passwd: true with no ConsolePassword, got:\n%s", s)
	}
	if strings.Contains(s, "chpasswd:") {
		t.Errorf("expected no chpasswd block with no ConsolePassword, got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataWithConsolePassword(t *testing.T) {
	p := Params{
		Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129",
		SoftwareRepo: "mabels/mseg-tester", ConsolePassword: "hunter2",
	}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "lock_passwd: false") {
		t.Errorf("expected lock_passwd: false with ConsolePassword set, got:\n%s", s)
	}
	if !strings.Contains(s, "ubuntu:hunter2") {
		t.Errorf("expected chpasswd to set the ubuntu account's password, got:\n%s", s)
	}
	if strings.Contains(s, "ssh_pwauth:") {
		t.Errorf("expected ConsolePassword to NOT enable SSH password auth, got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataWithoutConfigYAML(t *testing.T) {
	p := Params{Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129", SoftwareRepo: "mabels/mseg-tester"}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	// configLocalPath inside bootstrap.yaml legitimately mentions this
	// path regardless -- check specifically for a write_files entry
	// whose *path* is config.yaml, not just any mention of the string.
	if strings.Contains(string(out), "- path: /mseg-tester/config.yaml") {
		t.Errorf("expected no config.yaml write_files entry when ConfigYAML is empty, got:\n%s", out)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataWithConfigYAML(t *testing.T) {
	configYAML := "rebootDelay: \"10s\"\nsegments:\n  - name: \"129\"\n    dnsServer: \"192.168.129.5\"\n    dnsCheck: \"example.\"\n    routingCheck: \"1.1.1.1:443\"\n"
	p := Params{
		Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129",
		SoftwareRepo: "mabels/mseg-tester", ConfigYAML: configYAML,
	}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "path: /mseg-tester/config.yaml") {
		t.Errorf("expected a config.yaml write_files entry, got:\n%s", s)
	}
	if !strings.Contains(s, "      rebootDelay: \"10s\"") {
		t.Errorf("expected config.yaml content indented to 6 spaces, got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataWithoutEnvFile(t *testing.T) {
	p := Params{Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129", SoftwareRepo: "mabels/mseg-tester"}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	if strings.Contains(string(out), "- path: /etc/mseg-tester/.env") {
		t.Errorf("expected no .env write_files entry when EnvFile is empty, got:\n%s", out)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataWithEnvFile(t *testing.T) {
	p := Params{
		Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129",
		SoftwareRepo: "mabels/mseg-tester", EnvFile: "INFLUX_TOKEN=secret-value\n",
	}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "- path: /etc/mseg-tester/.env") {
		t.Errorf("expected a .env write_files entry, got:\n%s", s)
	}
	if !strings.Contains(s, `permissions: "0600"`) {
		t.Errorf("expected the .env entry to be 0600, got:\n%s", s)
	}
	if !strings.Contains(s, "      INFLUX_TOKEN=secret-value") {
		t.Errorf("expected .env content indented to 6 spaces, got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataNativeSegmentIsUpdateSegment(t *testing.T) {
	p := Params{
		Name: "verify-mseg-tester", TrunkIface: "ens18",
		NativeSegment: "128", UpdateSegment: "128", SoftwareRepo: "mabels/mseg-tester",
	}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "ens18.128:") {
		t.Errorf("expected no tagged VLAN sub-interface when native segment == update segment, got:\n%s", s)
	}
	if strings.Contains(s, "nativeSegment:") {
		t.Errorf("bootstrap.yaml no longer has a nativeSegment field (config.yaml's segment Type is now the single source of truth), got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataNativeSegmentDiffersFromUpdateSegment(t *testing.T) {
	p := Params{
		Name: "verify-mseg-tester", TrunkIface: "ens18",
		NativeSegment: "128", UpdateSegment: "129", SoftwareRepo: "mabels/mseg-tester",
	}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "ens18.129:") {
		t.Errorf("expected a tagged VLAN sub-interface for update segment 129, got:\n%s", s)
	}
	if !strings.Contains(s, "optional: true") {
		t.Errorf("expected the bare trunk interface marked optional (wait-online fix), got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataDefaultSoftwareRef(t *testing.T) {
	p := Params{Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129", SoftwareRepo: "mabels/mseg-tester"}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `softwareRef: "latest"`) {
		t.Errorf("expected softwareRef to default to \"latest\", got:\n%s", s)
	}
	if !strings.Contains(s, "GOBIN=/usr/local/bin go install") {
		t.Errorf("expected the bootstrap script to `go install`, got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestRenderUserDataWithSoftwareRef(t *testing.T) {
	p := Params{
		Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129",
		SoftwareRepo: "mabels/mseg-tester", SoftwareRef: "my-feature-branch",
	}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `softwareRef: "my-feature-branch"`) {
		t.Errorf("expected softwareRef to use the overridden SoftwareRef, got:\n%s", s)
	}
	assertValidYAML(t, out)
}

func TestNet0(t *testing.T) {
	cases := []struct {
		name string
		p    Params
		want string
	}{
		{
			name: "no vlans at all -- fully untagged",
			p:    Params{Bridge: "vmbr0"},
			want: "virtio,bridge=vmbr0",
		},
		{
			name: "tagged trunk, no native segment",
			p:    Params{Bridge: "vmbr0", TrunkVLANs: []string{"128", "129", "130"}},
			want: "virtio,bridge=vmbr0,trunks=128;129;130",
		},
		{
			// No "tag="/"trunks=" at all when NativeSegment is set --
			// TrunkVLANs is ignored in this branch. See net0's doc
			// comment: on an OVS bridge, Proxmox's "tag=" for a
			// genuinely-untagged VLAN goes through OVS's native-untagged
			// VLAN-database handling instead of leaving it alone, which
			// broke delivery for that segment in practice (confirmed
			// live against gw-to-earth-ng's OVN/OVS setup).
			name: "native segment plus other tagged segments -- no vlan params at all",
			p:    Params{Bridge: "vmbr1", TrunkVLANs: []string{"128", "129", "130", "131"}, NativeSegment: "128"},
			want: "virtio,bridge=vmbr1",
		},
		{
			name: "native segment only, nothing else tagged -- still no vlan params",
			p:    Params{Bridge: "vmbr1", TrunkVLANs: []string{"128"}, NativeSegment: "128"},
			want: "virtio,bridge=vmbr1",
		},
	}
	for _, c := range cases {
		if got := c.p.net0(); got != c.want {
			t.Errorf("%s: net0() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidateCreateNativeSegmentMustBeInTrunkVLANs(t *testing.T) {
	p := Params{
		Host: "root@x", VMID: 1, Name: "v", Storage: "s", Image: "i", Bridge: "b",
		UpdateSegment: "129", SoftwareRepo: "o/r", ConfigYAML: "segments: []\n",
		BIOS: "seabios", TrunkVLANs: []string{"129", "130"}, NativeSegment: "128",
	}
	if err := p.ValidateCreate(); err == nil {
		t.Error("expected an error when -native-segment isn't in -trunk-vlans")
	}
}

// fakeRunner is a scripted Runner for testing Step.Action functions
// without an actual ssh connection -- each Run call is matched against
// responses in order, and every command actually issued is recorded so
// tests can assert on it.
type fakeRunner struct {
	responses []fakeResponse
	calls     []string
	next      int
}

type fakeResponse struct {
	out string
	err error
}

func (f *fakeRunner) Run(command string, stdin []byte) (string, error) {
	f.calls = append(f.calls, command)
	if f.next >= len(f.responses) {
		return "", fmt.Errorf("fakeRunner: unexpected call #%d: %q (only %d response(s) scripted)", f.next+1, command, len(f.responses))
	}
	r := f.responses[f.next]
	f.next++
	return r.out, r.err
}

func minimalCreateParams() Params {
	return Params{
		Host: "root@proxmox", VMID: 199, Name: "verify-mseg-tester",
		Storage: "local-lvm", Image: "/img.qcow2", Bridge: "vmbr0",
		BIOS: "seabios", Start: false,
		UpdateSegment: "129", SoftwareRepo: "o/r", ConfigYAML: "segments: []\n",
		SnippetsStorage: "local", SnippetsPath: "/var/lib/vz/snippets",
	}
}

func TestBuildCreatePlanNoHostPCIByDefault(t *testing.T) {
	steps, err := minimalCreateParams().BuildCreatePlan()
	if err != nil {
		t.Fatalf("BuildCreatePlan: %v", err)
	}
	for _, s := range steps {
		if strings.Contains(s.Description, "hostpci") {
			t.Errorf("expected no hostpci step when HostPCIDevices is empty, got step %q", s.Description)
		}
	}
}

func TestBuildCreatePlanOneStepPerHostPCIDevice(t *testing.T) {
	p := minimalCreateParams()
	p.HostPCIDevices = []string{"14c3:0616", "8086:2725"}
	steps, err := p.BuildCreatePlan()
	if err != nil {
		t.Fatalf("BuildCreatePlan: %v", err)
	}
	var hostpciSteps []Step
	for _, s := range steps {
		if strings.Contains(s.Description, "hostpci") {
			hostpciSteps = append(hostpciSteps, s)
		}
	}
	if len(hostpciSteps) != 2 {
		t.Fatalf("expected 2 hostpci steps, got %d: %+v", len(hostpciSteps), hostpciSteps)
	}
	if !strings.Contains(hostpciSteps[0].Description, "14c3:0616") || !strings.Contains(hostpciSteps[0].Description, "hostpci0") {
		t.Errorf("expected first step to mention 14c3:0616/hostpci0, got %q", hostpciSteps[0].Description)
	}
	if !strings.Contains(hostpciSteps[1].Description, "8086:2725") || !strings.Contains(hostpciSteps[1].Description, "hostpci1") {
		t.Errorf("expected second step to mention 8086:2725/hostpci1, got %q", hostpciSteps[1].Description)
	}
	for _, s := range hostpciSteps {
		if s.Action == nil {
			t.Errorf("expected a hostpci step to be an Action (needs a live lookup), not a fixed Command, got %+v", s)
		}
	}
}

// createStepCommand returns the Command of the "qm create ..." step, or
// fails the test if it can't be found.
func createStepCommand(t *testing.T, steps []Step) string {
	t.Helper()
	for _, s := range steps {
		if strings.HasPrefix(s.Command, "qm create ") {
			return s.Command
		}
	}
	t.Fatalf("no \"qm create\" step found among %d steps", len(steps))
	return ""
}

func TestBuildCreatePlanAddsQ35WhenHostPCIDevicesSet(t *testing.T) {
	// Regression: `qm start` failed live with "q35 machine model is not
	// enabled" -- hostpciN's "pcie=1" flag (see attachHostPCI) requires
	// it, and BIOS "seabios" (this project's default) otherwise leaves
	// the VM on i440fx.
	p := minimalCreateParams()
	p.HostPCIDevices = []string{"14c3:0616"}
	steps, err := p.BuildCreatePlan()
	if err != nil {
		t.Fatalf("BuildCreatePlan: %v", err)
	}
	cmd := createStepCommand(t, steps)
	if !strings.Contains(cmd, "--machine q35") {
		t.Errorf("expected --machine q35 in the create command when HostPCIDevices is set, got: %s", cmd)
	}
	if strings.Contains(cmd, "--bios ovmf") || strings.Contains(cmd, "--efidisk0") {
		t.Errorf("expected seabios (no --bios/--efidisk0 override) when BIOS wasn't explicitly \"ovmf\", got: %s", cmd)
	}
}

func TestBuildCreatePlanNoQ35WithoutHostPCIDevicesOrOVMF(t *testing.T) {
	p := minimalCreateParams() // BIOS: "seabios", no HostPCIDevices
	steps, err := p.BuildCreatePlan()
	if err != nil {
		t.Fatalf("BuildCreatePlan: %v", err)
	}
	cmd := createStepCommand(t, steps)
	if strings.Contains(cmd, "--machine") {
		t.Errorf("expected no --machine override for a plain seabios VM with no passthrough, got: %s", cmd)
	}
}

func TestBuildCreatePlanOVMFStillGetsQ35AndEfidisk(t *testing.T) {
	p := minimalCreateParams()
	p.BIOS = "ovmf"
	steps, err := p.BuildCreatePlan()
	if err != nil {
		t.Fatalf("BuildCreatePlan: %v", err)
	}
	cmd := createStepCommand(t, steps)
	if !strings.Contains(cmd, "--machine q35") {
		t.Errorf("expected --machine q35 for BIOS ovmf, got: %s", cmd)
	}
	if !strings.Contains(cmd, "--bios ovmf") || !strings.Contains(cmd, "--efidisk0") {
		t.Errorf("expected --bios ovmf and --efidisk0 preserved, got: %s", cmd)
	}
	// Make sure --machine wasn't duplicated (both the HostPCIDevices and
	// the ovmf branch used to independently want to add it).
	if strings.Count(cmd, "--machine") != 1 {
		t.Errorf("expected exactly one --machine flag, got: %s", cmd)
	}
}

func TestBuildCreatePlanOVMFWithHostPCINoDuplicateMachineFlag(t *testing.T) {
	p := minimalCreateParams()
	p.BIOS = "ovmf"
	p.HostPCIDevices = []string{"14c3:0616"}
	steps, err := p.BuildCreatePlan()
	if err != nil {
		t.Fatalf("BuildCreatePlan: %v", err)
	}
	cmd := createStepCommand(t, steps)
	if strings.Count(cmd, "--machine") != 1 {
		t.Errorf("expected exactly one --machine flag even with both ovmf and hostpci devices set, got: %s", cmd)
	}
}

func TestAttachHostPCIParsesAddressAndAppliesIt(t *testing.T) {
	p := Params{Host: "root@proxmox", VMID: 199}
	r := &fakeRunner{responses: []fakeResponse{
		{out: "07:00.0 0280: 14c3:0616\n"},
		{out: ""},
	}}
	if err := p.attachHostPCI(r, 0, "14c3:0616"); err != nil {
		t.Fatalf("attachHostPCI: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 remote calls, got %d: %v", len(r.calls), r.calls)
	}
	if !strings.Contains(r.calls[0], "lspci -n -d") || !strings.Contains(r.calls[0], "14c3:0616") {
		t.Errorf("expected an lspci lookup for 14c3:0616, got %q", r.calls[0])
	}
	if !strings.Contains(r.calls[1], "qm set 199 --hostpci0") || !strings.Contains(r.calls[1], "0000:07:00.0,pcie=1") {
		t.Errorf("expected a qm set attaching the resolved+domain-prefixed address, got %q", r.calls[1])
	}
}

func TestAttachHostPCIPreservesExplicitDomain(t *testing.T) {
	p := Params{Host: "root@proxmox", VMID: 199}
	r := &fakeRunner{responses: []fakeResponse{
		{out: "0000:07:00.0 0280: 14c3:0616\n"},
		{out: ""},
	}}
	if err := p.attachHostPCI(r, 0, "14c3:0616"); err != nil {
		t.Fatalf("attachHostPCI: %v", err)
	}
	if !strings.Contains(r.calls[1], "0000:07:00.0,pcie=1") {
		t.Errorf("expected the already-domain-qualified address preserved as-is, got %q", r.calls[1])
	}
}

func TestAttachHostPCINoMatchIsAnError(t *testing.T) {
	p := Params{Host: "root@proxmox", VMID: 199}
	r := &fakeRunner{responses: []fakeResponse{{out: ""}}}
	if err := p.attachHostPCI(r, 0, "14c3:0616"); err == nil {
		t.Error("expected an error when lspci finds no matching device")
	}
}

func TestAttachHostPCILookupErrorPropagates(t *testing.T) {
	p := Params{Host: "root@proxmox", VMID: 199}
	r := &fakeRunner{responses: []fakeResponse{{err: fmt.Errorf("ssh: connection refused")}}}
	if err := p.attachHostPCI(r, 0, "14c3:0616"); err == nil {
		t.Error("expected the ssh error to propagate")
	}
}

// assertValidYAML parses the rendered cloud-config as YAML (not
// interpreting write_files' nested "content: |" blocks, just confirming
// the outer document itself is well-formed) -- catches indentation bugs
// in the template's conditional blocks (e.g. a bad "indent" call) that
// would otherwise only surface as a cloud-init failure on a real VM.
func assertValidYAML(t *testing.T, doc []byte) {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal(doc, &out); err != nil {
		t.Fatalf("rendered user-data is not valid YAML: %v\n%s", err, doc)
	}
}
