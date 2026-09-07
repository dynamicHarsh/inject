package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	setupworkflow "github.com/harsh-sonkar/env-pull/internal/setup"
	"github.com/harsh-sonkar/env-pull/internal/store"
)

var (
	setupInput            io.Reader = os.Stdin
	setupPackageRoot      string
	setupProjectID        string
	setupProvider         string
	setupAccount          string
	setupVault            string
	setupItemID           string
	setupItem             string
	setupBinding          string
	setupPackageScripts   []string
	setupCommand          []string
	setupValidation       []string
	setupLocal            bool
	setupSelectedInputs   []string
	setupConfirm          bool
	setupRemoveLegacyEnv  bool
	setupConfirmRemoveEnv bool
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure inject for a local or remote secret source",
	Long: `setup previews a non-secret inject.toml configuration and optional
command bindings. Selected package scripts are preserved under inject-owned names
so their existing package-manager commands continue to work. Setup validates the selected
provider through its existing CLI authentication context before changing project files.
A detected .env selects local credential-store
migration unless a remote provider or reference is selected.

Use --yes to apply the preview. Remote setup requires a finite validation command.
Use --remove-env together with --yes-remove-env to delete a detected legacy .env
after a successful validation.`,
	Args: cobra.NoArgs,
	RunE: func(command *cobra.Command, _ []string) error {
		directory := "."
		if setupPackageRoot != "" {
			directory = setupPackageRoot
		}
		packageScripts := append([]string(nil), setupPackageScripts...)
		if setupBinding != "" || len(setupCommand) > 0 {
			if len(packageScripts) > 0 {
				return fmt.Errorf("setup: package scripts and an explicit developer command cannot be selected together")
			}
			_, script, err := parsePackageScriptCommand(strings.Join(setupCommand, " "))
			if err != nil {
				return err
			}
			if setupBinding != "" && setupBinding != script {
				return fmt.Errorf("setup: binding %q does not match package script %q", setupBinding, script)
			}
			packageScripts = []string{script}
		}
		request := setupworkflow.Request{
			Directory:           directory,
			PackageRootExplicit: command.Flags().Changed("package-root"),
			ProjectID:           setupProjectID,
			Provider:            setupProvider,
			Account:             setupAccount,
			Vault:               setupVault,
			ItemID:              setupItemID,
			Item:                setupItem,
			PackageScripts:      packageScripts,
			Validate:            setupValidation,
			Local:               setupLocal,
			SelectedInputs:      setupSelectedInputs,
			Store:               store.NewSystem(),
			Confirm:             setupConfirm,
			RemoveLegacyEnv:     setupRemoveLegacyEnv,
			ConfirmRemoveEnv:    setupConfirmRemoveEnv,
			Output:              os.Stdout,
		}
		return runSetup(request, isTerminal(os.Stdin), isTerminal(os.Stdout), runInteractiveSetup, setupworkflow.Run)
	},
}

func runSetup(request setupworkflow.Request, stdinTerminal, stdoutTerminal bool, interactive, deterministic func(setupworkflow.Request) error) error {
	if stdinTerminal && stdoutTerminal {
		return interactive(request)
	}
	request.NonInteractive = true
	return deterministic(request)
}

func runInteractiveSetup(request setupworkflow.Request) error {
	if err := setupworkflow.ValidatePackageRoot(request.Directory, request.PackageRootExplicit); err != nil {
		return err
	}
	return runGuidedSetup(request, setupworkflow.Discover, promptSetup, setupworkflow.Run)
}

func isTerminal(file *os.File) bool {
	return term.IsTerminal(file.Fd())
}

func init() {
	setupCmd.Flags().StringVar(&setupPackageRoot, "package-root", "", "package directory to configure")
	setupCmd.Flags().StringVar(&setupProjectID, "project-id", "", "stable project identifier")
	setupCmd.Flags().StringVar(&setupProvider, "provider", "", "secret provider (1password or bitwarden)")
	setupCmd.Flags().StringVar(&setupAccount, "account", "", "1Password account")
	setupCmd.Flags().StringVar(&setupVault, "vault", "", "1Password vault")
	setupCmd.Flags().StringVar(&setupItemID, "item-id", "", "immutable remote item ID")
	setupCmd.Flags().StringVar(&setupItem, "item", "", "remote item name")
	setupCmd.Flags().StringVar(&setupBinding, "binding", "", "explicit command binding name")
	setupCmd.Flags().StringSliceVar(&setupPackageScripts, "package-script", nil, "package.json script to preserve through injection; repeat for each script")
	setupCmd.Flags().StringArrayVar(&setupCommand, "command", nil, "binding command argument; repeat for each argument")
	setupCmd.Flags().StringArrayVar(&setupValidation, "validate", nil, "finite validation command argument; repeat for each argument")
	setupCmd.Flags().BoolVar(&setupLocal, "local", false, "force import of a legacy .env into the local credential store")
	setupCmd.Flags().StringSliceVar(&setupSelectedInputs, "env-file", nil, "plaintext environment input to import; repeat for variants")
	setupCmd.Flags().BoolVar(&setupConfirm, "yes", false, "apply the previewed project changes")
	setupCmd.Flags().BoolVar(&setupRemoveLegacyEnv, "remove-env", false, "remove a legacy .env after validation")
	setupCmd.Flags().BoolVar(&setupConfirmRemoveEnv, "yes-remove-env", false, "confirm removal of a legacy .env")
	rootCmd.AddCommand(setupCmd)
}
