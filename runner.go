package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

type runnerState int

const (
	statePending runnerState = iota // waiting for capacity, booting, registering
	stateIdle                       // registered and listening for a job
	stateBusy                       // running a job
	stateDone                       // tearing down; no longer counted
)

func (s runnerState) String() string {
	return [...]string{"pending", "idle", "busy", "done"}[s]
}

// runner is one VM serving exactly one job.
type runner struct {
	name     string
	cancel   context.CancelFunc
	finished chan struct{} // closed once the VM is gone

	mu        sync.Mutex
	state     runnerState
	runnerID  int
	busySince time.Time
	completed chan struct{} // closed when GitHub reports the job finished
	closeOnce sync.Once
}

func (r *runner) getState() runnerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *runner) setState(s runnerState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateDone {
		return
	}
	r.state = s
	if s == stateBusy {
		r.busySince = time.Now()
	}
}

func (r *runner) markCompleted() { r.closeOnce.Do(func() { close(r.completed) }) }

const (
	guestAgentTimeout = 3 * time.Minute
	listenTimeout     = 5 * time.Minute
	pollInterval      = 2 * time.Second
	timeoutInterval   = 30 * time.Second
	failureBackoff    = 15 * time.Second
	cleanupTimeout    = 2 * time.Minute
)

// guestLauncher runs inside the VM via `tart exec`, with the runner's JIT
// config on stdin, and stays in the foreground: the exec session lasts
// exactly as long as the runner, which exits after its single job. The
// guest agent kills anything left behind when the session ends, so the
// runner can't be detached anyway.
//
// $1 is the runner directory and $2 the user to run as. The JIT config is a
// registration credential, so it arrives on stdin rather than in argv.
//
// Jobs get git that never prompts, as on GitHub-hosted runners. The images
// configure Git Credential Manager, which otherwise waits forever for a
// sign-in nobody can give, and a job that runs git without credentials
// hangs until its timeout. sudo resets the environment, so on that path the
// variables go through env.
const guestLauncher = `set -eu
IFS= read -r JIT
cd "$1"
if [ "$(id -u)" -eq 0 ] && [ "$2" != root ]; then
  exec sudo -u "$2" -H env GCM_INTERACTIVE=never GIT_TERMINAL_PROMPT=0 ACTIONS_RUNNER_INPUT_JITCONFIG="$JIT" ./run.sh < /dev/null
fi
GCM_INTERACTIVE=never GIT_TERMINAL_PROMPT=0 ACTIONS_RUNNER_INPUT_JITCONFIG="$JIT" exec ./run.sh < /dev/null
`

// runVM takes a runner from capacity to teardown. It never returns until the
// VM is deleted and its registration is gone.
func (c *poolController) runVM(ctx context.Context, r *runner) {
	log := c.log.With(slog.String("runner", r.name))
	defer close(r.finished)

	err := c.serve(ctx, log, r)
	switch {
	case ctx.Err() != nil:
		log.Info("Runner stopped")
	case err != nil:
		log.Error("Runner failed", slog.Any("error", err))
		// Keep counting this runner as pending for a moment so a broken
		// image or GitHub outage doesn't become a tight clone/boot loop.
		// Scaling down can still cancel it.
		r.mu.Lock()
		if r.state != stateDone {
			r.state = statePending
		}
		r.runnerID = 0
		r.mu.Unlock()
		select {
		case <-time.After(failureBackoff):
		case <-ctx.Done():
		}
	}
	r.setState(stateDone)
	c.forget(r)
}

func (c *poolController) serve(ctx context.Context, log *slog.Logger, r *runner) error {
	lease, err := c.host.Acquire(ctx, c.pool.MemoryMB, c.pool.CPU, c.pool.isMacOS())
	if err != nil {
		return err
	}
	defer c.host.Release(lease)

	// Everything below runs on cleanup even if ctx is canceled.
	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	}

	log.Info("Creating VM", slog.String("image", c.pool.Image), slog.Int("slot", lease.Slot))
	start := time.Now()
	if err := c.tart.Clone(ctx, c.pool.Image, r.name); err != nil {
		return err
	}
	defer func() {
		cctx, cancel := cleanupCtx()
		defer cancel()
		_ = c.tart.Stop(cctx, r.name)
		if err := c.tart.Delete(cctx, r.name); err != nil {
			log.Error("Failed to delete VM", slog.Any("error", err))
			return
		}
		log.Info("VM deleted")
	}()
	if err := c.tart.Configure(ctx, r.name, c.pool.CPU, c.pool.MemoryMB, c.pool.DiskGB, lease.Slot); err != nil {
		return err
	}

	vmLog, err := os.Create(filepath.Join(c.logDir, r.name+".log"))
	if err != nil {
		return err
	}
	defer func() {
		vmLog.Close()
		os.Remove(vmLog.Name())
	}()
	exited, err := c.tart.Start(r.name, c.pool.tartRunArgs(), vmLog)
	if err != nil {
		return err
	}
	if err := c.waitGuestAgent(ctx, r.name, exited); err != nil {
		return err
	}
	log.Info("VM booted", slog.Duration("took", time.Since(start).Round(time.Second)))

	jit, err := c.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: r.name}, c.scaleSet.ID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	r.mu.Lock()
	r.runnerID = jit.Runner.ID
	r.mu.Unlock()
	defer func() {
		// An ephemeral runner deregisters itself after its job; this
		// catches the ones that never got that far. The VM goes first so
		// GitHub doesn't refuse because a job is still running on it.
		cctx, cancel := cleanupCtx()
		defer cancel()
		_ = c.tart.Stop(cctx, r.name)
		err := c.client.RemoveRunner(cctx, int64(jit.Runner.ID))
		if err != nil && !isNotFound(err) {
			log.Warn("Failed to remove runner registration", slog.Any("error", err))
		}
	}()

	ready := make(chan struct{})
	runnerOut := &watchWriter{w: vmLog, needle: []byte("Listening for Jobs"), found: ready}
	runnerExited, err := c.tart.ExecAsync(r.name, strings.NewReader(jit.EncodedJITConfig+"\n"), runnerOut,
		"sh", "-c", guestLauncher, "eph", c.pool.RunnerDir, c.pool.RunnerUser)
	if err != nil {
		return fmt.Errorf("start runner: %w", err)
	}
	listenDeadline := time.After(listenTimeout)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
	case err := <-runnerExited:
		return fmt.Errorf("runner exited before connecting to GitHub (%v); runner log:\n%s", err, logTail(vmLog.Name()))
	case err := <-exited:
		return fmt.Errorf("VM exited before the runner connected (%v); runner log:\n%s", err, logTail(vmLog.Name()))
	case <-listenDeadline:
		return fmt.Errorf("runner did not connect within %s; runner log:\n%s", listenTimeout, logTail(vmLog.Name()))
	}
	r.setState(stateIdle)
	log.Info("Runner ready", slog.Duration("took", time.Since(start).Round(time.Second)))

	timeoutCheck := time.NewTicker(timeoutInterval)
	defer timeoutCheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.completed:
			log.Info("Job completed")
			return nil
		case err := <-runnerExited:
			log.Info("Runner exited", slog.Any("exit", err))
			return nil
		case err := <-exited:
			log.Info("VM shut down", slog.Any("exit", err))
			return nil
		case <-timeoutCheck.C:
			r.mu.Lock()
			since := r.busySince
			r.mu.Unlock()
			if !since.IsZero() && c.jobTimeout > 0 && time.Since(since) > c.jobTimeout {
				return fmt.Errorf("job exceeded job_timeout of %s", c.jobTimeout)
			}
		}
	}
}

func (c *poolController) waitGuestAgent(ctx context.Context, name string, exited <-chan error) error {
	deadline := time.Now().Add(guestAgentTimeout)
	for {
		if _, err := c.tart.Exec(ctx, name, nil, "true"); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("guest agent not reachable after %s: %w", guestAgentTimeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exited:
			return fmt.Errorf("VM exited while booting: %v", err)
		case <-time.After(pollInterval):
		}
	}
}

// watchWriter copies to w and closes found the first time needle appears.
type watchWriter struct {
	w      io.Writer
	needle []byte
	found  chan struct{}
	tail   []byte
	seen   bool
}

func (w *watchWriter) Write(p []byte) (int, error) {
	if !w.seen {
		// Keep enough of the previous write to catch a needle split
		// across two writes.
		buf := append(w.tail, p...)
		if bytes.Contains(buf, w.needle) {
			w.seen = true
			close(w.found)
		} else {
			w.tail = append([]byte(nil), buf[max(0, len(buf)-len(w.needle)):]...)
		}
	}
	return w.w.Write(p)
}

func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(unavailable)"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return strings.Join(lines[max(0, len(lines)-20):], "\n")
}
