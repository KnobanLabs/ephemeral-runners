// Command ephemeral-runners autoscales GitHub Actions runners on this Mac,
// one fresh Tart VM per job, deleted as soon as the job is done. One
// controller serves every deployment (org, repo or enterprise) configured
// on the Mac, sharing its capacity between them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/actions/scaleset"
)

const usage = `usage: ephemeral-runners [-config-dir dir] <command>

commands:
  run      register every deployment's scale sets and serve jobs until stopped
  check    validate the config, credentials, images and budget, then exit
  cleanup  delete leftover VMs and their runner registrations, then exit
  remove-scale-sets <deployment.toml>
           delete one deployment's runner scale sets from GitHub, to retire
           it; take it out of the controller first (scripts/remove-deployment.sh)
  validate-config <deployment.toml>...
           check deployment files offline (no GitHub, Tart or keys needed),
           e.g. in a config repo's CI

The config dir holds host.toml (optional) and deployments/<name>.toml.
`

func main() {
	configDir := flag.String("config-dir", defaultConfigDir(), "config directory")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if flag.Arg(0) == "validate-config" {
		if flag.NArg() < 2 {
			flag.Usage()
			os.Exit(2)
		}
		if err := validateConfig(flag.Args()[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "remove-scale-sets" {
		if flag.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		log := HostConfig{LogLevel: "info"}.Logger()
		if err := removeScaleSets(ctx, flag.Arg(1), log); err != nil {
			log.Error("Exiting", slog.Any("error", err))
			os.Exit(1)
		}
		return
	}

	cfg, err := LoadConfig(*configDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := cfg.Host.Logger()

	switch flag.Arg(0) {
	case "run":
		err = run(ctx, cfg, log)
	case "check":
		err = check(ctx, cfg, log)
	case "cleanup":
		err = cleanup(ctx, cfg, log)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("Exiting", slog.Any("error", err))
		os.Exit(1)
	}
}

func newTart() (*Tart, error) {
	bin, err := exec.LookPath("tart")
	if err != nil {
		return nil, fmt.Errorf("tart not found on PATH: %w", err)
	}
	return &Tart{Bin: bin}, nil
}

func newClient(ctx context.Context, d *Deployment) (*scaleset.Client, error) {
	info := scaleset.SystemInfo{System: "ephemeral-runners", Subsystem: "tart", Version: "0.2.0", CommitSHA: "NA"}
	if app := d.GitHubApp; app != nil {
		key, err := os.ReadFile(app.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("deployment %q: read GitHub App private key: %w", d.Name, err)
		}
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: d.RegistrationURL,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID:       app.ClientID,
				InstallationID: app.InstallationID,
				PrivateKey:     string(key),
			},
			SystemInfo: info,
		})
	}
	out, err := exec.CommandContext(ctx, "sh", "-c", d.TokenCommand).Output()
	if err != nil {
		return nil, fmt.Errorf("deployment %q: token_command failed: %w", d.Name, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return nil, fmt.Errorf("deployment %q: token_command printed nothing", d.Name)
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     d.RegistrationURL,
		PersonalAccessToken: token,
		SystemInfo:          info,
	})
}

func newClients(ctx context.Context, cfg *Config) (map[string]*scaleset.Client, error) {
	clients := map[string]*scaleset.Client{}
	for _, d := range cfg.Deployments {
		c, err := newClient(ctx, d)
		if err != nil {
			return nil, err
		}
		clients[d.Name] = c
	}
	return clients, nil
}

func runnerGroupID(ctx context.Context, client *scaleset.Client, name string) (int, error) {
	if isDefaultGroup(name) {
		return 1, nil
	}
	group, err := client.GetRunnerGroupByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("runner group %q: %w", name, err)
	}
	return group.ID, nil
}

func logBudget(log *slog.Logger, cfg *Config, b HostBudget) {
	names := make([]string, len(cfg.Deployments))
	for i, d := range cfg.Deployments {
		names[i] = d.Name
	}
	log.Info("Host budget", slog.Int("memoryMB", b.MemoryMB), slog.Int("vcpus", b.VCPUs),
		slog.Int("maxMacOSVMs", cfg.Host.MaxMacOSVMs), slog.Any("deployments", names))
}

func run(ctx context.Context, cfg *Config, log *slog.Logger) error {
	tart, err := newTart()
	if err != nil {
		return err
	}
	if cfg.usesIsolatedNetwork() {
		if err := CheckSoftnet(); err != nil {
			return err
		}
	}
	clients, err := newClients(ctx, cfg)
	if err != nil {
		return err
	}
	if err := checkImages(ctx, cfg, tart); err != nil {
		return err
	}
	if err := cleanupLeftovers(ctx, log, tart, clients, cfg.Host.VMPrefix); err != nil {
		return fmt.Errorf("clean up leftover VMs: %w", err)
	}

	logDir := filepath.Join(os.TempDir(), "ephemeral-runners")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}

	budget, err := cfg.ResolveBudget()
	if err != nil {
		return err
	}
	logBudget(log, cfg, budget)
	host := NewHost(budget.MemoryMB, budget.VCPUs, cfg.Host.MaxMacOSVMs)
	// Runners get their own context so shutdown can drain them after the
	// listeners have stopped taking new jobs.
	runnersCtx, stopRunners := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRunners()

	var controllers []*poolController
	for _, d := range cfg.Deployments {
		client := clients[d.Name]
		groupID, err := runnerGroupID(ctx, client, d.RunnerGroup)
		if err != nil {
			return fmt.Errorf("deployment %q: %w", d.Name, err)
		}
		for _, pool := range d.Pools {
			ss, err := ensureScaleSet(ctx, client, groupID, pool)
			if err != nil {
				return fmt.Errorf("deployment %q: %w", d.Name, err)
			}
			log.Info("Scale set ready", slog.String("deployment", d.Name), slog.String("name", ss.Name),
				slog.Int("id", ss.ID), slog.Int("maxRunners", pool.MaxRunners), slog.String("image", pool.Image))
			controllers = append(controllers, &poolController{
				deployment:  d.Name,
				pool:        pool,
				prefix:      cfg.Host.VMPrefix,
				jobTimeout:  d.JobTimeout.Duration,
				logDir:      logDir,
				owner:       hostname() + "/" + d.Name + "/" + pool.Name,
				client:      client,
				scaleSet:    ss,
				host:        host,
				tart:        tart,
				log:         log.With(slog.String("deployment", d.Name), slog.String("pool", pool.Name)),
				listenerLog: cfg.Host.ListenerLogger().With(slog.String("deployment", d.Name), slog.String("pool", pool.Name)),
				runnersCtx:  runnersCtx,
				runners:     map[string]*runner{},
			})
		}
	}

	var wg sync.WaitGroup
	for _, c := range controllers {
		wg.Go(func() { c.listen(ctx) })
	}
	<-ctx.Done()
	wg.Wait()

	log.Info("Shutting down", slog.Duration("drainTimeout", cfg.Host.DrainTimeout.Duration))
	for _, c := range controllers {
		wg.Go(func() { c.drain(cfg.Host.DrainTimeout.Duration) })
	}
	wg.Wait()
	log.Info("All runners stopped")
	return nil
}

// checkImages fails fast when a pool's golden image hasn't been pulled, which
// would otherwise make every clone download it.
func checkImages(ctx context.Context, cfg *Config, tart *Tart) error {
	have := map[string]bool{}
	for _, source := range []string{"oci", "local"} {
		vms, err := tart.List(ctx, source)
		if err != nil {
			return err
		}
		for _, vm := range vms {
			have[vm.Name] = true
		}
	}
	for _, d := range cfg.Deployments {
		for _, p := range d.Pools {
			if !have[p.Image] {
				return fmt.Errorf("%s/%s: image %s is not present locally; run `tart pull %s` first", d.Name, p.Name, p.Image, p.Image)
			}
		}
	}
	return nil
}

func check(ctx context.Context, cfg *Config, log *slog.Logger) error {
	tart, err := newTart()
	if err != nil {
		return err
	}
	if cfg.usesIsolatedNetwork() {
		if err := CheckSoftnet(); err != nil {
			return err
		}
		log.Info("Softnet ready for isolated networking")
	}
	budget, err := cfg.ResolveBudget()
	if err != nil {
		return err
	}
	logBudget(log, cfg, budget)
	if err := checkImages(ctx, cfg, tart); err != nil {
		return err
	}
	log.Info("Images present")
	for _, d := range cfg.Deployments {
		client, err := newClient(ctx, d)
		if err != nil {
			return err
		}
		groupID, err := runnerGroupID(ctx, client, d.RunnerGroup)
		if err != nil {
			return fmt.Errorf("deployment %q: %w", d.Name, err)
		}
		for _, p := range d.Pools {
			ss, err := client.GetRunnerScaleSet(ctx, groupID, p.Name)
			if err != nil {
				return fmt.Errorf("deployment %q: GitHub API check failed: %w", d.Name, err)
			}
			state := "will be created on first run"
			if ss != nil {
				state = fmt.Sprintf("exists (id %d)", ss.ID)
			}
			log.Info("Scale set", slog.String("deployment", d.Name), slog.String("name", p.Name), slog.String("state", state))
		}
	}
	log.Info("Config OK")
	return nil
}

func cleanup(ctx context.Context, cfg *Config, log *slog.Logger) error {
	tart, err := newTart()
	if err != nil {
		return err
	}
	clients, err := newClients(ctx, cfg)
	if err != nil {
		return err
	}
	return cleanupLeftovers(ctx, log, tart, clients, cfg.Host.VMPrefix)
}

func removeScaleSets(ctx context.Context, path string, log *slog.Logger) error {
	d, err := LoadDeployment(path)
	if err != nil {
		return err
	}
	client, err := newClient(ctx, d)
	if err != nil {
		return err
	}
	groupID, err := runnerGroupID(ctx, client, d.RunnerGroup)
	if err != nil {
		return err
	}
	for _, p := range d.Pools {
		ss, err := client.GetRunnerScaleSet(ctx, groupID, p.Name)
		if err != nil {
			return err
		}
		if ss == nil {
			log.Info("Scale set not found", slog.String("deployment", d.Name), slog.String("name", p.Name))
			continue
		}
		if err := client.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
			return fmt.Errorf("delete scale set %q: %w", p.Name, err)
		}
		log.Info("Scale set deleted", slog.String("deployment", d.Name), slog.String("name", p.Name), slog.Int("id", ss.ID))
	}
	return nil
}

// validateConfig loads deployment files the way the controller would, and
// checks them against each other, without touching GitHub, Tart or keys.
func validateConfig(paths []string) error {
	cfg := &Config{Host: HostConfig{MaxMacOSVMs: 2, VMPrefix: "eph"}}
	var errs []error
	for _, p := range paths {
		d, err := LoadDeployment(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cfg.Deployments = append(cfg.Deployments, d)
	}
	if len(errs) == 0 {
		errs = append(errs, cfg.Validate())
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, d := range cfg.Deployments {
		fmt.Printf("%s: OK (%d pools)\n", d.Path, len(d.Pools))
	}
	return nil
}
