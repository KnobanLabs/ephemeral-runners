package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/actions/scaleset"
)

// Config is everything one controller serves, loaded from a directory:
//
//	host.toml                this Mac's settings (optional)
//	deployments/<name>.toml  one per org, repo or enterprise served
//
// Deployments share the host's capacity.
type Config struct {
	Dir         string
	Host        HostConfig
	Deployments []*Deployment
}

// HostConfig is this Mac's settings, shared by every deployment.
type HostConfig struct {
	// MemoryBudgetMB is the total memory all VMs together may be given.
	// 0 means the host's RAM minus HostReservedMB.
	MemoryBudgetMB int `toml:"memory_budget_mb"`
	// HostReservedMB is memory kept back for macOS when the budget is
	// derived from the host's RAM.
	HostReservedMB int `toml:"host_reserved_mb"`
	// VCPUBudget is the total vCPUs all VMs together may be given. 0 means
	// twice the host's physical cores: VMs rarely keep every vCPU busy,
	// but beyond that jobs slow each other down more than they gain.
	VCPUBudget int `toml:"vcpu_budget"`
	// MaxMacOSVMs caps concurrent macOS guests across all deployments.
	// Apple's Virtualization.framework refuses to run more than 2.
	MaxMacOSVMs int `toml:"max_macos_vms"`

	// VMPrefix marks VMs owned by this controller. Any local Tart VM whose
	// name starts with "<prefix>-" is deleted on startup.
	VMPrefix string `toml:"vm_prefix"`
	// DrainTimeout is how long shutdown waits for busy runners to finish
	// before their VMs are destroyed.
	DrainTimeout Duration `toml:"drain_timeout"`
	LogLevel     string   `toml:"log_level"`
}

// Deployment is one org, repo or enterprise this Mac runs jobs for.
type Deployment struct {
	// Name comes from the file name. It appears in VM names, so it is
	// limited to lowercase letters and digits.
	Name string `toml:"-"`
	Path string `toml:"-"`

	// RegistrationURL is the org, repo or enterprise the scale sets
	// register with, e.g. https://github.com/my-org.
	RegistrationURL string `toml:"registration_url"`
	// RunnerGroup is the org runner group the scale sets belong to.
	// "Default" always means the org's own default group, not one an
	// enterprise passes down under the same name.
	RunnerGroup string `toml:"runner_group"`

	// TokenCommand prints a GitHub token on stdout. It is run with sh -c,
	// so the token never has to be written into a file.
	TokenCommand string     `toml:"token_command"`
	GitHubApp    *AppConfig `toml:"github_app"`

	// JobTimeout bounds how long a VM may stay busy with one job.
	JobTimeout Duration `toml:"job_timeout"`

	Pools []Pool `toml:"pool"`
}

// AppConfig authenticates as a GitHub App instead of with a token.
type AppConfig struct {
	ClientID       string `toml:"client_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKeyPath string `toml:"private_key_path"`
}

// Pool is one runner scale set backed by one golden image.
type Pool struct {
	// Name is the scale set name, and what workflows put in runs-on.
	Name   string   `toml:"name"`
	Labels []string `toml:"labels"`
	Image  string   `toml:"image"`
	// OS is "macos" or "linux".
	OS       string `toml:"os"`
	CPU      int    `toml:"cpu"`
	MemoryMB int    `toml:"memory_mb"`
	// DiskGB grows the VM's disk; 0 keeps the image's size. Clones are
	// copy-on-write, so this costs only what jobs actually write.
	DiskGB     int `toml:"disk_gb"`
	MaxRunners int `toml:"max_runners"`
	// MinIdle keeps this many booted runners waiting for jobs. Each one
	// holds its memory while idle.
	MinIdle int `toml:"min_idle"`
	// RunnerDir is where the actions runner lives inside the image.
	RunnerDir string `toml:"runner_dir"`
	// RunnerUser is the guest user the runner runs as.
	RunnerUser string `toml:"runner_user"`
	// Network is "isolated" (the default) or "shared". Isolated VMs use
	// Tart's Softnet: they can reach the internet, and not your local
	// network, other VMs or private address ranges. Shared is Tart's plain
	// NAT, which can reach everything this Mac can.
	Network string `toml:"network"`
	// NetworkAllow lists IPv4 CIDRs an isolated VM may reach anyway, e.g.
	// a package mirror on the LAN.
	NetworkAllow []string `toml:"network_allow"`
	// DNS lists the IPv4 DNS servers an isolated macOS VM uses. macOS guests
	// on Softnet can't reach the gateway, which DHCP names as their DNS
	// server, so they get public resolvers instead (defaultGuestDNS).
	DNS []string `toml:"dns"`
	// RunArgs are extra arguments for `tart run`, e.g. ["--rosetta", "rosetta"].
	RunArgs []string `toml:"run_args"`
}

func (p Pool) isMacOS() bool { return p.OS == "macos" }

func (p Pool) isolated() bool { return p.Network == "isolated" }

// defaultGuestDNS is Cloudflare's and Google's public DNS.
var defaultGuestDNS = []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4"}

// tartRunArgs are the pool's arguments for `tart run`.
func (p Pool) tartRunArgs() []string {
	var args []string
	if p.isolated() {
		args = append(args, "--net-softnet")
		if len(p.NetworkAllow) > 0 {
			args = append(args, "--net-softnet-allow="+strings.Join(p.NetworkAllow, ","))
		}
	}
	return append(args, p.RunArgs...)
}

// Duration lets TOML values like "6h" decode into a time.Duration.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

var deploymentNameRE = regexp.MustCompile(`^[a-z0-9]+$`)

func defaultConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ephemeral-runners")
}

// LoadConfig reads host.toml and every deployments/*.toml under dir.
func LoadConfig(dir string) (*Config, error) {
	cfg := &Config{
		Dir: dir,
		Host: HostConfig{
			HostReservedMB: 8 * 1024,
			MaxMacOSVMs:    2,
			VMPrefix:       "eph",
			LogLevel:       "info",
		},
	}
	hostPath := filepath.Join(dir, "host.toml")
	if _, err := toml.DecodeFile(hostPath, &cfg.Host); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", hostPath, err)
	}

	paths, err := filepath.Glob(filepath.Join(dir, "deployments", "*.toml"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	var errs []error
	for _, path := range paths {
		d, err := LoadDeployment(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cfg.Deployments = append(cfg.Deployments, d)
	}
	if len(paths) == 0 {
		errs = append(errs, fmt.Errorf("no deployments in %s", filepath.Join(dir, "deployments")))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, cfg.Validate()
}

// DeploymentName derives a deployment's name from its file: the file name
// without .toml, or for <name>/config.toml, <name>.
func DeploymentName(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".toml")
	if base == "config" {
		return filepath.Base(filepath.Dir(path))
	}
	return base
}

func LoadDeployment(path string) (*Deployment, error) {
	d := &Deployment{
		Name:        DeploymentName(path),
		Path:        path,
		RunnerGroup: "Default",
		JobTimeout:  Duration{6 * time.Hour},
	}
	if _, err := toml.DecodeFile(path, d); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if app := d.GitHubApp; app != nil && strings.HasPrefix(app.PrivateKeyPath, "~/") {
		home, _ := os.UserHomeDir()
		app.PrivateKeyPath = filepath.Join(home, app.PrivateKeyPath[2:])
	}
	for i := range d.Pools {
		p := &d.Pools[i]
		if p.Network == "" {
			p.Network = "isolated"
		}
		if p.DNS == nil && p.isolated() && p.isMacOS() {
			p.DNS = defaultGuestDNS
		}
		if p.RunnerUser == "" {
			p.RunnerUser = "admin"
		}
		if p.RunnerDir == "" {
			if p.isMacOS() {
				p.RunnerDir = "/Users/" + p.RunnerUser + "/actions-runner"
			} else {
				p.RunnerDir = "/home/" + p.RunnerUser + "/actions-runner"
			}
		}
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("deployment %q (%s): %w", d.Name, path, err)
	}
	return d, nil
}

func (c *Config) Validate() error {
	h := c.Host
	var errs []error
	if h.MemoryBudgetMB < 0 || h.VCPUBudget < 0 || h.HostReservedMB < 0 {
		errs = append(errs, errors.New("memory_budget_mb, vcpu_budget and host_reserved_mb must not be negative"))
	}
	if h.MaxMacOSVMs < 0 || h.MaxMacOSVMs > 2 {
		errs = append(errs, errors.New("max_macos_vms must be between 0 and 2"))
	}
	if strings.ContainsAny(h.VMPrefix, " -/") || h.VMPrefix == "" {
		errs = append(errs, errors.New("vm_prefix must be non-empty and contain no spaces, dashes or slashes"))
	}
	// Two deployments registering the same scale set would fight over its
	// message session.
	owner := map[string]string{}
	for _, d := range c.Deployments {
		for _, p := range d.Pools {
			key := strings.ToLower(strings.TrimSuffix(d.RegistrationURL, "/")) + " " + p.Name
			if other, ok := owner[key]; ok {
				errs = append(errs, fmt.Errorf("deployments %q and %q both define pool %q for %s", other, d.Name, p.Name, d.RegistrationURL))
			}
			owner[key] = d.Name
		}
	}
	return errors.Join(errs...)
}

func (d *Deployment) Validate() error {
	var errs []error
	if !deploymentNameRE.MatchString(d.Name) {
		errs = append(errs, fmt.Errorf("deployment name %q must be lowercase letters and digits only (it comes from the file name)", d.Name))
	}
	if d.RegistrationURL == "" {
		errs = append(errs, errors.New("registration_url is required"))
	}
	if strings.Contains(d.RegistrationURL, "YOUR-") {
		errs = append(errs, errors.New("registration_url still has a YOUR-* placeholder"))
	}
	if d.TokenCommand == "" && d.GitHubApp == nil {
		errs = append(errs, errors.New("one of token_command or [github_app] is required"))
	}
	if app := d.GitHubApp; app != nil {
		if app.ClientID == "" || strings.Contains(app.ClientID, "YOUR-") || app.InstallationID <= 0 || app.PrivateKeyPath == "" {
			errs = append(errs, errors.New("[github_app] needs client_id, installation_id and private_key_path"))
		}
	}
	if len(d.Pools) == 0 {
		errs = append(errs, errors.New("at least one [[pool]] is required"))
	}
	seen := map[string]bool{}
	for _, p := range d.Pools {
		if p.Name == "" || p.Image == "" {
			errs = append(errs, fmt.Errorf("pool %q: name and image are required", p.Name))
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("pool %q: duplicate name", p.Name))
		}
		seen[p.Name] = true
		if p.OS != "macos" && p.OS != "linux" {
			errs = append(errs, fmt.Errorf("pool %q: os must be \"macos\" or \"linux\"", p.Name))
		}
		if p.Network != "isolated" && p.Network != "shared" {
			errs = append(errs, fmt.Errorf("pool %q: network must be \"isolated\" or \"shared\"", p.Name))
		}
		for _, cidr := range p.NetworkAllow {
			if _, _, err := net.ParseCIDR(cidr); err != nil || strings.Contains(cidr, ":") {
				errs = append(errs, fmt.Errorf("pool %q: network_allow entry %q must be an IPv4 CIDR", p.Name, cidr))
			}
		}
		for _, ip := range p.DNS {
			if a := net.ParseIP(ip); a == nil || a.To4() == nil {
				errs = append(errs, fmt.Errorf("pool %q: dns entry %q must be an IPv4 address", p.Name, ip))
			}
		}
		if len(p.DNS) > 0 && !(p.isolated() && p.isMacOS()) {
			errs = append(errs, fmt.Errorf("pool %q: dns only applies to isolated macOS pools", p.Name))
		}
		if len(p.NetworkAllow) > 0 && !p.isolated() {
			errs = append(errs, fmt.Errorf("pool %q: network_allow only applies to isolated pools", p.Name))
		}
		for _, a := range p.RunArgs {
			if strings.HasPrefix(a, "--net-") {
				errs = append(errs, fmt.Errorf("pool %q: set networking with network and network_allow, not %s in run_args", p.Name, a))
			}
		}
		if p.CPU <= 0 || p.MemoryMB <= 0 || p.MaxRunners <= 0 {
			errs = append(errs, fmt.Errorf("pool %q: cpu, memory_mb and max_runners must be positive", p.Name))
		}
		if p.MinIdle < 0 || p.MinIdle > p.MaxRunners {
			errs = append(errs, fmt.Errorf("pool %q: min_idle must be between 0 and max_runners", p.Name))
		}
	}
	return errors.Join(errs...)
}

// ListenerLogger is for the scaleset listener, which logs every long poll.
// It stays quiet unless log_level is debug.
func (h HostConfig) ListenerLogger() *slog.Logger {
	if strings.EqualFold(h.LogLevel, "debug") {
		return h.Logger()
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (h HostConfig) Logger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(h.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// isDefaultGroup reports whether name refers to the org's own default runner
// group, which always has ID 1.
func isDefaultGroup(name string) bool {
	return strings.EqualFold(name, scaleset.DefaultRunnerGroup)
}

// HostBudget is the capacity VMs may use on this machine.
type HostBudget struct {
	MemoryMB int
	VCPUs    int
}

// ResolveBudget fills in budgets left at 0 from this Mac's hardware, and
// checks that every pool's VM fits.
func (c *Config) ResolveBudget() (HostBudget, error) {
	b := HostBudget{MemoryMB: c.Host.MemoryBudgetMB, VCPUs: c.Host.VCPUBudget}
	if b.MemoryMB == 0 {
		total, err := sysctlInt("hw.memsize")
		if err != nil {
			return b, err
		}
		b.MemoryMB = int(total/(1024*1024)) - c.Host.HostReservedMB
	}
	if b.VCPUs == 0 {
		cores, err := sysctlInt("hw.physicalcpu")
		if err != nil {
			return b, err
		}
		b.VCPUs = int(cores) * 2
	}
	var errs []error
	for _, d := range c.Deployments {
		for _, p := range d.Pools {
			if p.MemoryMB > b.MemoryMB || p.CPU > b.VCPUs {
				errs = append(errs, fmt.Errorf("%s/%s: one VM (%d MB, %d vCPU) doesn't fit the budget (%d MB, %d vCPU)",
					d.Name, p.Name, p.MemoryMB, p.CPU, b.MemoryMB, b.VCPUs))
			}
		}
	}
	return b, errors.Join(errs...)
}

func sysctlInt(name string) (int64, error) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return 0, fmt.Errorf("sysctl %s: %w", name, err)
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// usesIsolatedNetwork reports whether any pool needs Softnet.
func (c *Config) usesIsolatedNetwork() bool {
	for _, d := range c.Deployments {
		for _, p := range d.Pools {
			if p.isolated() {
				return true
			}
		}
	}
	return false
}
