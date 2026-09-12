package subprocess

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRunReturnsStdoutAndExplainsFailures(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	out, err := Run(ctx, Command{Bin: "sh", Args: []string{"-c", "echo hello"}})
	if err != nil || out != "hello\n" {
		t.Fatalf("got %q, %v", out, err)
	}

	_, err = Run(ctx, Command{Bin: "sh", Args: []string{"-c", "echo the reason >&2; exit 3"}})
	if err == nil || !strings.Contains(err.Error(), "the reason") || !strings.HasPrefix(err.Error(), "sh -c") {
		t.Fatalf("the error should carry the command and stderr, got %v", err)
	}

	// With nothing on stderr, stdout is the best explanation available.
	_, err = Run(ctx, Command{Bin: "sh", Args: []string{"-c", "echo only stdout; exit 1"}})
	if err == nil || !strings.Contains(err.Error(), "only stdout") {
		t.Fatalf("got %v", err)
	}

	out, err = Run(ctx, Command{Bin: "sh", Args: []string{"-c", "pwd"}, Dir: t.TempDir()})
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("Dir should be honoured: %q %v", out, err)
	}
}

func TestRunTimesOutAndSaysSo(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep")
	}
	start := time.Now()
	_, err := Run(context.Background(), Command{Bin: "sleep", Args: []string{"30"}, Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout did not cut the command short")
	}
}
