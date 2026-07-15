package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/store"
)

// Daemonize re-execs the current binary in the foreground mode, detached in
// its own session, with output going to the state-dir log file. The parent
// returns the child PID and exits; the child is the actual daemon. Baseline
// self-managed lifecycle as the baseline — platform service files just skip this by
// running `start --foreground` themselves.
func Daemonize(ctx context.Context, paths config.Paths) (int, error) {
	const op = "daemon.Daemonize"
	if err := paths.EnsureDirs(); err != nil {
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	// Refuse up front when a live daemon holds the lock, so `cognosis start`
	// itself exits nonzero rather than spawning a child that dies in its log.
	// The authoritative guarantee is still the child's AcquireLock.
	if pid, err := ReadLockPID(paths.LockFile()); err == nil && processAlive(pid) {
		return 0, cogerr.Ef(op, cogerr.Conflict,
			"another cognosis daemon is running (pid %d, lock %s)", pid, paths.LockFile())
	}
	logPath := filepath.Join(paths.StateDir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	defer func() { _ = logFile.Close() }()

	exe, err := os.Executable()
	if err != nil {
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	cmd := exec.CommandContext(ctx, exe, "start", "--foreground")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	// Deliberately not waiting: the child owns its own lifetime now.
	return cmd.Process.Pid, cmd.Process.Release()
}

// Stop signals the running daemon (SIGTERM) and waits for the lock to clear.
func Stop(paths config.Paths, timeout time.Duration) error {
	const op = "daemon.Stop"
	lockPath := paths.LockFile()
	pid, err := ReadLockPID(lockPath)
	if err != nil {
		if cogerr.Is(err, cogerr.NotFound) {
			return cogerr.Ef(op, cogerr.NotFound, "no daemon running (no lock at %s)", lockPath)
		}
		return err
	}
	if !processAlive(pid) {
		_ = os.Remove(lockPath) // stale
		return cogerr.Ef(op, cogerr.NotFound, "daemon (pid %d) already gone; removed stale lock", pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cogerr.Ef(op, cogerr.Internal, "daemon (pid %d) did not exit within %s", pid, timeout)
}

// Check is one named health check result for `cognosis status`, which answers
// "is this actually healthy", not just "is the process alive".
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Status runs the startup health checks without mutating anything.
func Status(ctx context.Context, cfg *config.Config) []Check {
	var checks []Check

	// Process.
	pid, err := ReadLockPID(cfg.Paths().LockFile())
	switch {
	case err == nil && processAlive(pid):
		checks = append(checks, Check{"daemon", true, fmt.Sprintf("running (pid %d)", pid)})
	case err == nil:
		checks = append(checks, Check{"daemon", false, fmt.Sprintf("stale lock (pid %d not running)", pid)})
	default:
		checks = append(checks, Check{"daemon", false, "not running"})
	}

	// Postgres.
	dsn, err := store.ResolveDSN(cfg.DSN)
	if err != nil {
		checks = append(checks, Check{"postgres", false, err.Error()})
		return checks // schema check needs a DSN too
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s, err := store.Connect(cctx, dsn)
	if err != nil {
		checks = append(checks, Check{"postgres", false, err.Error()})
	} else {
		s.Close()
		checks = append(checks, Check{"postgres", true, "reachable"})
	}

	// Schema currency.
	if st, err := store.Status(dsn); err != nil {
		checks = append(checks, Check{"schema", false, err.Error()})
	} else {
		checks = append(checks, Check{"schema", st.Current(),
			fmt.Sprintf("version %d, latest %d, pending %d, dirty %v", st.Version, st.Latest, st.Pending, st.Dirty)})
	}

	// MCP server listening (only meaningful when the daemon runs).
	if pid, err := ReadLockPID(cfg.Paths().LockFile()); err == nil && processAlive(pid) {
		dialCtx, dialCancel := context.WithTimeout(ctx, 2*time.Second)
		var d net.Dialer
		conn, err := d.DialContext(dialCtx, "tcp", cfg.BindAddress)
		dialCancel()
		if err != nil {
			checks = append(checks, Check{"mcp", false, fmt.Sprintf("not listening on %s: %v", cfg.BindAddress, err)})
		} else {
			_ = conn.Close()
			checks = append(checks, Check{"mcp", true, "listening on " + cfg.BindAddress})
		}
	}

	// Embedding provider reachability.
	if cfg.Embedding.Provider == "ollama" {
		prov := embed.NewOllama(cfg.Embedding.URL, cfg.Embedding.Model)
		ectx, ecancel := context.WithTimeout(ctx, 3*time.Second)
		defer ecancel()
		if err := prov.Health(ectx); err != nil {
			checks = append(checks, Check{"embedding", false, err.Error()})
		} else {
			checks = append(checks, Check{"embedding", true,
				fmt.Sprintf("ollama reachable at %s (%s)", cfg.Embedding.URL, cfg.Embedding.Model)})
		}
	} else {
		checks = append(checks, Check{"embedding", false,
			fmt.Sprintf("unknown provider %q", cfg.Embedding.Provider)})
	}
	return checks
}
