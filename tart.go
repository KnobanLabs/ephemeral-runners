package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Tart wraps the tart CLI.
type Tart struct {
	Bin string
}

func (t *Tart) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, t.Bin, args...)
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("tart %s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

func (t *Tart) Clone(ctx context.Context, image, name string) error {
	_, err := t.run(ctx, nil, "clone", image, name)
	return err
}

// Configure sets the VM's CPU and memory and pins its MAC address. Every
// clone otherwise gets a fresh MAC, and macOS's DHCP server keeps a lease
// for each one until its pool runs dry.
func (t *Tart) Configure(ctx context.Context, name string, cpu, memoryMB, diskGB, slot int) error {
	args := []string{"set", name, "--cpu", strconv.Itoa(cpu), "--memory", strconv.Itoa(memoryMB)}
	if diskGB > 0 {
		args = append(args, "--disk-size", strconv.Itoa(diskGB))
	}
	if _, err := t.run(ctx, nil, args...); err != nil {
		return err
	}
	path := filepath.Join(tartHome(), "vms", name, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read VM config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse VM config: %w", err)
	}
	// Locally administered unicast range, one address per slot.
	cfg["macAddress"] = fmt.Sprintf("02:eb:00:00:%02x:%02x", slot>>8, slot&0xff)
	data, err = json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Start boots the VM headless, without audio and without a keyboard or
// pointer. The returned channel yields once the VM has shut down, however that
// happens.
//
// Tart passes audio through to the host by default, input included, so every
// VM would ask macOS for the Mac's microphone. Jobs need no sound, and no one
// types into or points at a runner's VM, so it gets no input devices either.
func (t *Tart) Start(name string, extraArgs []string, log io.Writer) (<-chan error, error) {
	args := append([]string{"run", name, "--no-graphics", "--no-audio", "--no-keyboard", "--no-pointer"}, extraArgs...)
	cmd := exec.Command(t.Bin, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tart run: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// Exec runs argv inside the VM through the Tart guest agent. stdin, when
// non-nil, is streamed to the guest command.
func (t *Tart) Exec(ctx context.Context, name string, stdin io.Reader, argv ...string) ([]byte, error) {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, name)
	args = append(args, argv...)
	return t.run(ctx, stdin, args...)
}

func (t *Tart) Stop(ctx context.Context, name string) error {
	_, err := t.run(ctx, nil, "stop", "--timeout", "5", name)
	return err
}

func (t *Tart) Delete(ctx context.Context, name string) error {
	_, err := t.run(ctx, nil, "delete", name)
	return err
}

type tartVM struct {
	Name   string `json:"Name"`
	Source string `json:"Source"`
	State  string `json:"State"`
}

func (t *Tart) List(ctx context.Context, source string) ([]tartVM, error) {
	out, err := t.run(ctx, nil, "list", "--source", source, "--format", "json")
	if err != nil {
		return nil, err
	}
	var vms []tartVM
	if err := json.Unmarshal(out, &vms); err != nil {
		return nil, fmt.Errorf("parse tart list: %w", err)
	}
	return vms, nil
}

func tartHome() string {
	if h := os.Getenv("TART_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tart")
}

// ExecAsync starts argv inside the VM and returns at once. The channel
// yields when the guest command exits or the VM goes away.
func (t *Tart) ExecAsync(name string, stdin io.Reader, out io.Writer, argv ...string) (<-chan error, error) {
	args := append([]string{"exec", "-i", name}, argv...)
	cmd := exec.Command(t.Bin, args...)
	cmd.Stdin = stdin
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tart exec: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// CheckSoftnet fails unless Softnet can isolate VMs: Tart runs it from PATH,
// and it needs root to set up the VM network (it drops root straight after),
// which comes from being owned by root with the setuid bit. A Homebrew
// upgrade installs a fresh binary without it.
func CheckSoftnet() error {
	path, err := exec.LookPath("softnet")
	if err != nil {
		return fmt.Errorf("softnet not found on PATH, needed for isolated networking: brew install openai/tools/softnet")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || info.Mode()&os.ModeSetuid == 0 {
		return fmt.Errorf("softnet (%s) must be owned by root with the setuid bit for isolated networking; run: sudo chown root:wheel %s && sudo chmod u+s %s", real, real, real)
	}
	return nil
}
