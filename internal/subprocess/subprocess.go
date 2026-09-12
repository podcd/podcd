// Package subprocess runs the external tools the agent shells out to (git,
// podman, systemctl, journalctl) with one set of rules: a deadline, captured
// output, and an error that says which command failed and what it printed.
package subprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Command is one invocation.
type Command struct {
	Bin  string
	Args []string
	Dir  string   // working directory; "" for the current one
	Env  []string // full environment; nil inherits the process's
	// Timeout bounds the run; zero means no deadline beyond ctx.
	Timeout time.Duration
}

// Run executes the command and returns its stdout. On failure the error
// carries the command line and the tool's own message (stderr, else stdout).
func Run(ctx context.Context, c Command) (string, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.Bin, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(stdout.String())
	}
	if msg == "" {
		msg = err.Error()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg += " (timed out)"
	}
	return stdout.String(), fmt.Errorf("%s %s: %s", filepath.Base(c.Bin), strings.Join(c.Args, " "), msg)
}
