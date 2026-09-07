package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestSetupRequiresPrimaryPackageScriptOutsideTTY(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDirectory := t.TempDir()
	manifestPath := filepath.Join(projectDirectory, "package.json")
	manifest := []byte(`{"name":"billing-api","packageManager":"npm@10.0.0","scripts":{"dev":"vite"}}`)
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binaryPath, "setup",
		"--project-id", "billing-api",
		"--provider", "1password",
		"--account", "acme",
		"--vault", "Engineering",
		"--item-id", "stable-note-id",
		"--validate=true",
	)
	command.Dir = projectDirectory
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "primary package-script binding is required") {
		t.Fatalf("inject setup error = %v, output = %q; want required primary binding", err, output)
	}
	if got, readErr := os.ReadFile(manifestPath); readErr != nil || !bytes.Equal(got, manifest) {
		t.Errorf("package.json = %q, %v; want unchanged", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(projectDirectory, "inject.toml")); !os.IsNotExist(statErr) {
		t.Errorf("inject.toml stat error = %v, want no configuration", statErr)
	}
}

func TestSetupRequiresExplicitPackageRootForWorkspace(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	workspaceDirectory := t.TempDir()
	rootManifest := []byte(`{"name":"workspace","packageManager":"npm@10.0.0","workspaces":["packages/*"],"scripts":{"dev":"root-dev"}}`)
	if err := os.WriteFile(filepath.Join(workspaceDirectory, "package.json"), rootManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	packageDirectory := filepath.Join(workspaceDirectory, "packages", "api")
	if err := os.MkdirAll(packageDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDirectory, "package.json"), []byte(`{"name":"api","packageManager":"npm@10.0.0","scripts":{"dev":"api-dev"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	baseArgs := []string{"setup", "--provider", "1password", "--account", "acme", "--vault", "Engineering", "--item-id", "stable-note-id", "--package-script", "dev", "--validate=true"}
	command := exec.Command(binaryPath, baseArgs...)
	command.Dir = workspaceDirectory
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "package root is ambiguous") {
		t.Fatalf("workspace setup error = %v, output = %q; want ambiguous package root", err, output)
	}
	if _, statErr := os.Stat(filepath.Join(workspaceDirectory, "inject.toml")); !os.IsNotExist(statErr) {
		t.Errorf("workspace inject.toml stat error = %v, want no configuration", statErr)
	}

	args := append(baseArgs, "--package-root", filepath.Join("packages", "api"))
	command = exec.Command(binaryPath, args...)
	command.Dir = workspaceDirectory
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("selected package setup: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Project ID: api") {
		t.Errorf("selected package output = %q, want API package plan", output)
	}
}

func TestSetupRejectsUnsupportedDeveloperCommandsWithoutMutation(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	for _, arguments := range [][]string{
		{"dev"},
		{"go", "run", "."},
		{"npm", "run", "dev", "|", "tee", "app.log"},
		{"npm", "run", "dev", ">", "app.log"},
		{"npm", "run", "dev", "&&", "npm", "test"},
	} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			projectDirectory := t.TempDir()
			manifestPath := filepath.Join(projectDirectory, "package.json")
			manifest := []byte(`{"name":"billing-api","packageManager":"npm@10.0.0","scripts":{"dev":"vite"}}`)
			if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"setup", "--provider", "1password", "--account", "acme", "--vault", "Engineering", "--item-id", "stable-note-id", "--binding", "dev", "--validate=true"}
			for _, argument := range arguments {
				args = append(args, "--command="+argument)
			}
			command := exec.Command(binaryPath, args...)
			command.Dir = projectDirectory
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "unsupported developer command") {
				t.Fatalf("inject setup error = %v, output = %q; want unsupported command", err, output)
			}
			if got, readErr := os.ReadFile(manifestPath); readErr != nil || !bytes.Equal(got, manifest) {
				t.Errorf("package.json = %q, %v; want unchanged", got, readErr)
			}
			if _, statErr := os.Stat(filepath.Join(projectDirectory, "inject.toml")); !os.IsNotExist(statErr) {
				t.Errorf("inject.toml stat error = %v, want no configuration", statErr)
			}
		})
	}
}

func TestInteractiveSetupConfirmsOrCollectsPrimaryCommandAndCancelsWithoutMutation(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	for _, test := range []struct {
		name           string
		packageManager string
		scripts        string
		primaryInput   string
	}{
		{name: "confirm discovered candidate", packageManager: "npm", scripts: `{"dev":"vite"}`},
		{name: "collect command without candidate", packageManager: "pnpm", scripts: `{"release":"shipit"}`, primaryInput: "pnpm run release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			projectDirectory := t.TempDir()
			manifestPath := filepath.Join(projectDirectory, "package.json")
			manifest := []byte(`{"name":"billing-api","packageManager":"` + test.packageManager + `@10.0.0","scripts":` + test.scripts + `}`)
			if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDirectory, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binaryPath, "setup")
			command.Dir = projectDirectory
			command.Env = append(os.Environ(), "TERM=dumb")
			terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 40, Cols: 120})
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()

			var output bytes.Buffer
			respondToPrompt(t, terminal, &output, "Project ID", "\r")
			respondToPrompt(t, terminal, &output, "Secret source", "\r")
			respondToPrompt(t, terminal, &output, "Environment inputs and profiles", "\r")
			respondToPrompt(t, terminal, &output, "Primary developer command", test.primaryInput+"\r")
			respondToPrompt(t, terminal, &output, "Additional developer commands", "\r")
			respondToPrompt(t, terminal, &output, "Finite validation command", "\r")
			respondToPrompt(t, terminal, &output, "Remove .env after successful validation?", "\r")
			respondToPrompt(t, terminal, &output, "Review setup", "n\r")

			_, _ = io.Copy(&output, terminal)
			if err := command.Wait(); err != nil {
				t.Fatalf("interactive setup: %v\n%s", err, output.String())
			}
			if !strings.Contains(output.String(), "Developer commands: "+strings.TrimPrefix(test.primaryInput, test.packageManager+" run ")) && test.primaryInput != "" {
				t.Errorf("review did not show collected primary command:\n%s", output.String())
			}
			if got, readErr := os.ReadFile(manifestPath); readErr != nil || !bytes.Equal(got, manifest) {
				t.Errorf("package.json = %q, %v; want unchanged", got, readErr)
			}
			if _, statErr := os.Stat(filepath.Join(projectDirectory, "inject.toml")); !os.IsNotExist(statErr) {
				t.Errorf("inject.toml stat error = %v, want no configuration", statErr)
			}
		})
	}
}

func respondToPrompt(t *testing.T, terminal *os.File, output *bytes.Buffer, prompt, response string) {
	t.Helper()
	start := output.Len()
	buffer := make([]byte, 1024)
	for !bytes.Contains(output.Bytes()[start:], []byte(prompt)) {
		count, err := terminal.Read(buffer)
		if count > 0 {
			_, _ = output.Write(buffer[:count])
		}
		if err != nil {
			t.Fatalf("wait for prompt %q: %v\n%s", prompt, err, output.String())
		}
	}
	if _, err := terminal.Write([]byte(response)); err != nil {
		t.Fatalf("respond to prompt %q: %v", prompt, err)
	}
}
