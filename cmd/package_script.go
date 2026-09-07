package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/harsh-sonkar/env-pull/internal/project"
)

var packageScriptCmd = &cobra.Command{
	Use:                "__run-package-script <name> [args...]",
	Hidden:             true,
	DisableFlagParsing: true,
	Args:               cobra.MinimumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		config, err := project.Find()
		if err != nil {
			return fmt.Errorf("package script: %w", err)
		}
		binding, err := config.ScriptBinding(args[0])
		if err != nil {
			return fmt.Errorf("package script: %w", err)
		}
		secrets, err := loadProfileSecrets(binding.Profile, false)
		if err != nil {
			return err
		}
		manager, err := invokingPackageManager(binding.PackageManager)
		if err != nil {
			return fmt.Errorf("package script: %w", err)
		}
		for _, script := range []string{binding.PreScript, binding.Script, binding.PostScript} {
			if script == "" {
				continue
			}
			command := []string{manager, "run", script}
			if script == binding.Script && len(args) > 1 {
				command = append(command, "--")
				command = append(command, args[1:]...)
			}
			if err := runChild(command, secrets); err != nil {
				return fmt.Errorf("package script %q: %w", args[0], err)
			}
		}
		return nil
	},
}

func invokingPackageManager(fallback string) (string, error) {
	executable := os.Getenv("npm_execpath")
	executableFamily := packageManagerFromExecutable(executable)
	userAgent := os.Getenv("npm_config_user_agent")
	userAgentFamily, _, _ := strings.Cut(userAgent, "/")
	if !supportedPackageManager(userAgentFamily) {
		userAgentFamily = ""
	}
	if executableFamily != "" && userAgentFamily != "" && executableFamily != userAgentFamily {
		return "", fmt.Errorf("package manager metadata is ambiguous")
	}
	family := executableFamily
	if family == "" {
		family = userAgentFamily
	}
	if family == "" {
		family = fallback
	}
	if !supportedPackageManager(family) {
		return "", fmt.Errorf("package manager is not detected")
	}
	if executableFamily == family {
		if path, err := exec.LookPath(executable); err == nil {
			return path, nil
		}
	}
	path, err := exec.LookPath(family)
	if err != nil {
		return "", fmt.Errorf("package manager %q is unavailable", family)
	}
	return path, nil
}

func packageManagerFromExecutable(executable string) string {
	name := strings.ToLower(filepath.Base(strings.ReplaceAll(executable, `\`, "/")))
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "npm", "npm-cli.js":
		return "npm"
	case "pnpm", "pnpm.cjs":
		return "pnpm"
	case "yarn", "yarn.js", "yarnpkg":
		return "yarn"
	case "bun":
		return "bun"
	default:
		return ""
	}
}

func supportedPackageManager(family string) bool {
	switch family {
	case "npm", "pnpm", "yarn", "bun":
		return true
	default:
		return false
	}
}

func init() {
	rootCmd.AddCommand(packageScriptCmd)
}
