package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPools = `
[[pool]]
name = "mac"
os = "macos"
image = "ghcr.io/example/macos:latest"
cpu = 4
memory_mb = 8192
max_runners = 2

[[pool]]
name = "ubuntu-arm"
os = "linux"
image = "ghcr.io/example/ubuntu:24.04"
cpu = 4
memory_mb = 4096
max_runners = 8
`

func deploymentTOML(url string) string {
	return `registration_url = "` + url + `"
[github_app]
client_id = "Iv1.test"
installation_id = 1
private_key_path = "~/key.pem"
` + testPools
}

func writeConfigDir(t *testing.T, host string, deployments map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deployments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if host != "" {
		if err := os.WriteFile(filepath.Join(dir, "host.toml"), []byte(host), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range deployments {
		if err := os.WriteFile(filepath.Join(dir, "deployments", name+".toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadConfigMultipleDeployments(t *testing.T) {
	dir := writeConfigDir(t, `vm_prefix = "ci"
memory_budget_mb = 20000
vcpu_budget = 16
`, map[string]string{
		"acme":   deploymentTOML("https://github.com/acme"),
		"globex": deploymentTOML("https://github.com/globex"),
	})
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Deployments) != 2 || cfg.Deployments[0].Name != "acme" || cfg.Deployments[1].Name != "globex" {
		t.Fatalf("deployments = %+v", cfg.Deployments)
	}
	if cfg.Host.VMPrefix != "ci" || cfg.Host.MaxMacOSVMs != 2 {
		t.Fatalf("host = %+v", cfg.Host)
	}
	home, _ := os.UserHomeDir()
	if got := cfg.Deployments[0].GitHubApp.PrivateKeyPath; got != filepath.Join(home, "key.pem") {
		t.Fatalf("key path not expanded: %s", got)
	}
	if got := cfg.Deployments[1].Pools[1].RunnerDir; got != "/home/admin/actions-runner" {
		t.Fatalf("runner dir default = %s", got)
	}
	b, err := cfg.ResolveBudget()
	if err != nil || b.MemoryMB != 20000 || b.VCPUs != 16 {
		t.Fatalf("budget = %+v, %v", b, err)
	}
}

func TestLoadConfigRejectsSameScaleSetTwice(t *testing.T) {
	dir := writeConfigDir(t, "", map[string]string{
		"one": deploymentTOML("https://github.com/acme"),
		"two": deploymentTOML("https://github.com/Acme/"),
	})
	_, err := LoadConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "both define pool") {
		t.Fatalf("expected duplicate scale set error, got %v", err)
	}
}

func TestLoadConfigRejectsBadDeploymentName(t *testing.T) {
	dir := writeConfigDir(t, "", map[string]string{"acme-corp": deploymentTOML("https://github.com/acme")})
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "lowercase letters and digits") {
		t.Fatalf("expected name error, got %v", err)
	}
}

func TestBudgetMustFitEveryPool(t *testing.T) {
	dir := writeConfigDir(t, "memory_budget_mb = 6000\nvcpu_budget = 16\n", map[string]string{
		"acme": deploymentTOML("https://github.com/acme"),
	})
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ResolveBudget(); err == nil || !strings.Contains(err.Error(), "acme/mac") {
		t.Fatalf("expected the 8 GB macOS pool not to fit, got %v", err)
	}
}

func TestRepoConfigs(t *testing.T) {
	if _, err := LoadDeployment("config.example.toml"); err == nil || !strings.Contains(err.Error(), "YOUR-") {
		t.Fatalf("example config should be rejected until filled in, got %v", err)
	}
	if _, err := LoadConfig(writeHostExample(t)); err != nil {
		t.Errorf("host.example.toml: %v", err)
	}
}

func TestValidateConfig(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "acme", "config.toml")
	b := filepath.Join(dir, "globex.toml")
	os.MkdirAll(filepath.Dir(a), 0o700)
	os.WriteFile(a, []byte(deploymentTOML("https://github.com/acme")), 0o600)
	os.WriteFile(b, []byte(deploymentTOML("https://github.com/globex")), 0o600)
	if err := validateConfig([]string{a, b}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(b, []byte(deploymentTOML("https://github.com/acme")), 0o600)
	if err := validateConfig([]string{a, b}); err == nil || !strings.Contains(err.Error(), "both define pool") {
		t.Fatalf("expected duplicate scale set error, got %v", err)
	}
	if err := validateConfig([]string{"config.example.toml"}); err == nil {
		t.Fatal("placeholders should fail validation")
	}
}

// writeHostExample builds a config dir from host.example.toml plus one
// valid deployment, so the example's settings are checked too.
func writeHostExample(t *testing.T) string {
	t.Helper()
	host, err := os.ReadFile("host.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	return writeConfigDir(t, string(host), map[string]string{"acme": deploymentTOML("https://github.com/acme")})
}

func TestDeploymentName(t *testing.T) {
	for path, want := range map[string]string{
		"deployments/acme/config.toml":                            "acme",
		"/home/x/.config/ephemeral-runners/deployments/acme.toml": "acme",
	} {
		if got := DeploymentName(path); got != want {
			t.Errorf("DeploymentName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestVMNameRoundTrip(t *testing.T) {
	name := vmName("eph", "acme", "ubuntu-arm", "1a2b3c4d")
	if name != "eph-acme-ubuntu-arm-1a2b3c4d" {
		t.Fatalf("vmName = %s", name)
	}
	if d, ok := vmDeployment("eph", name); !ok || d != "acme" {
		t.Fatalf("vmDeployment = %q, %v", d, ok)
	}
	for _, other := range []string{"ephx-acme-mac-1", "my-vm", "ghcr.io/cirruslabs/macos-tahoe-xcode:latest"} {
		if _, ok := vmDeployment("eph", other); ok {
			t.Errorf("%q should not belong to this controller", other)
		}
	}
}

func TestPoolNetwork(t *testing.T) {
	dir := writeConfigDir(t, "", map[string]string{"acme": deploymentTOML("https://github.com/acme")})
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Deployments[0].Pools[0]
	if p.Network != "isolated" || !cfg.usesIsolatedNetwork() {
		t.Fatalf("pools should default to isolated networking, got %q", p.Network)
	}
	if got := strings.Join(p.tartRunArgs(), " "); got != "--net-softnet" {
		t.Fatalf("tartRunArgs = %q", got)
	}
	p.NetworkAllow = []string{"192.168.1.50/32"}
	p.RunArgs = []string{"--rosetta", "rosetta"}
	if got := strings.Join(p.tartRunArgs(), " "); got != "--net-softnet --net-softnet-allow=192.168.1.50/32 --rosetta rosetta" {
		t.Fatalf("tartRunArgs = %q", got)
	}
	if got := strings.Join(cfg.Deployments[0].Pools[0].DNS, ","); got != "1.1.1.1,1.0.0.1,8.8.8.8,8.8.4.4" {
		t.Fatalf("isolated macOS pool DNS = %q", got)
	}
	if dns := cfg.Deployments[0].Pools[1].DNS; dns != nil {
		t.Fatalf("Linux pool should get no DNS override, got %v", dns)
	}
	p = Pool{Network: "shared", RunArgs: []string{"--rosetta", "rosetta"}}
	if got := strings.Join(p.tartRunArgs(), " "); got != "--rosetta rosetta" {
		t.Fatalf("shared tartRunArgs = %q", got)
	}

	for snippet, want := range map[string]string{
		"network = \"bridged\"\n":                                  `network must be "isolated" or "shared"`,
		"network_allow = [\"192.168.1.0/33\"]\n":                   "must be an IPv4 CIDR",
		"network = \"shared\"\nnetwork_allow = [\"10.0.0.0/8\"]\n": "only applies to isolated pools",
		"run_args = [\"--net-bridged\", \"en0\"]\n":                "not --net-bridged in run_args",
	} {
		body := strings.Replace(deploymentTOML("https://github.com/acme"), "max_runners = 2\n", "max_runners = 2\n"+snippet, 1)
		dir := writeConfigDir(t, "", map[string]string{"acme": body})
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: expected %q, got %v", snippet, want, err)
		}
	}
}

func TestDNSOnlyForIsolatedMacOS(t *testing.T) {
	body := strings.Replace(deploymentTOML("https://github.com/acme"), `os = "linux"`, "os = \"linux\"\ndns = [\"1.1.1.1\"]", 1)
	dir := writeConfigDir(t, "", map[string]string{"acme": body})
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "dns only applies to isolated macOS pools") {
		t.Fatalf("expected dns on a Linux pool to be rejected, got %v", err)
	}
}
