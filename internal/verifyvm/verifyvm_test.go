package verifyvm

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderUserDataWithoutConfigYAML(t *testing.T) {
	p := Params{Name: "verify-mseg-tester", TrunkIface: "ens18", UpdateSegment: "129", SoftwareRepo: "mabels/mseg-tester"}
	out, err := p.RenderUserData()
	if err != nil {
		t.Fatalf("RenderUserData: %v", err)
	}
	// configLocalPath inside bootstrap.yaml legitimately mentions this
	// path regardless -- check specifically for a write_files entry
	// whose *path* is config.yaml, not just any mention of the string.
	if strings.Contains(string(out), "- path: /etc/mseg-tester/config.yaml") {
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
	if !strings.Contains(s, "path: /etc/mseg-tester/config.yaml") {
		t.Errorf("expected a config.yaml write_files entry, got:\n%s", s)
	}
	if !strings.Contains(s, "      rebootDelay: \"10s\"") {
		t.Errorf("expected config.yaml content indented to 6 spaces, got:\n%s", s)
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
	if !strings.Contains(s, "nativeSegment: \"128\"") {
		t.Errorf("expected nativeSegment recorded in bootstrap.yaml, got:\n%s", s)
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
		name   string
		p      Params
		want   string
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
			name: "native segment plus other tagged segments",
			p:    Params{Bridge: "vmbr1", TrunkVLANs: []string{"128", "129", "130", "131"}, NativeSegment: "128"},
			want: "virtio,bridge=vmbr1,tag=128,trunks=129;130;131",
		},
		{
			name: "native segment only, nothing else tagged",
			p:    Params{Bridge: "vmbr1", TrunkVLANs: []string{"128"}, NativeSegment: "128"},
			want: "virtio,bridge=vmbr1,tag=128",
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
