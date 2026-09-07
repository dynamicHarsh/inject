package executor

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// RunCommand executes the given command as a child process, injecting secrets
// into its environment via OS-level process tree inheritance. It merges the
// current process environment with the provided secrets map, then spawns the
// child with stdin/stdout/stderr bound to the caller — acting as a transparent
// wrapper. The child's exact exit code is preserved inside any returned
// *exec.ExitError so callers can forward it faithfully.
//
// Secrets are never written to disk or logged; they exist only in the child's
// in-memory environment and vanish when the process terminates.
func RunCommand(command []string, secrets map[string]string) error {
	if len(command) == 0 {
		return fmt.Errorf("executor: command must not be empty")
	}

	// Build the child environment: inherit the current environment, then
	// overlay the secrets. Later entries override earlier ones on all platforms.
	env := os.Environ()
	for k, v := range secrets {
		env = append(env, k+"="+v)
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("executor: command failed: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case received := <-signals:
				_ = cmd.Process.Signal(received)
			case <-done:
				return
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("executor: command failed: %w", err)
	}
	return nil
}
