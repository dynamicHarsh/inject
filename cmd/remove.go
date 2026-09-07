package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/harsh-sonkar/env-pull/internal/project"
	"github.com/harsh-sonkar/env-pull/internal/setup"
	"github.com/harsh-sonkar/env-pull/internal/store"
)

var removeConfirm bool

var removeCmd = &cobra.Command{
	Use:   "remove --yes",
	Short: "Remove this project's local configuration and credentials",
	Args:  cobra.NoArgs,
	RunE: func(command *cobra.Command, _ []string) error {
		return runRemove(".", store.NewSystem(), removeConfirm, command.OutOrStdout())
	},
}

func runRemove(directory string, credentialStore store.Store, confirmed bool, output io.Writer) error {
	configPath := filepath.Join(directory, project.FileName)
	config, err := project.Load(configPath)
	if err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	if err := previewRemoval(configPath, config, credentialStore, output); err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("remove: pass --yes to remove this project")
	}
	return removeConfig(configPath, config, credentialStore)
}

func previewRemoval(configPath string, config project.Config, credentialStore store.Store, output io.Writer) error {
	fmt.Fprintln(output, "The following local state will be removed:")
	for name, binding := range config.ScriptBindings {
		fmt.Fprintf(output, "- restore package.json script %q\n", name)
		fmt.Fprintf(output, "- delete reserved package.json entry %q\n", binding.Script)
		if binding.PreScript != "" {
			fmt.Fprintf(output, "- restore package.json lifecycle hook %q\n", "pre"+name)
			fmt.Fprintf(output, "- delete reserved package.json entry %q\n", binding.PreScript)
		}
		if binding.PostScript != "" {
			fmt.Fprintf(output, "- restore package.json lifecycle hook %q\n", "post"+name)
			fmt.Fprintf(output, "- delete reserved package.json entry %q\n", binding.PostScript)
		}
	}
	fmt.Fprintf(output, "- configuration %q\n", configPath)
	for profileName, profile := range config.Profiles {
		if profile.Provider == "local" {
			if _, err := credentialStore.Get(config.ProjectID, profileName); err == nil {
				fmt.Fprintf(output, "- local credential-store entry for profile %q\n", profileName)
			} else if err != store.ErrUnavailable {
				return fmt.Errorf("remove: inspect local profile %q: %w", profileName, err)
			}
			continue
		}
		if _, _, err := credentialStore.GetCache(config.ProjectID, profileName); err == nil {
			fmt.Fprintf(output, "- remote cache for profile %q\n", profileName)
		} else if err != store.ErrUnavailable {
			return fmt.Errorf("remove: inspect remote cache for profile %q: %w", profileName, err)
		}
	}
	return nil
}

func removeConfig(configPath string, config project.Config, credentialStore store.Store) (removeErr error) {
	var manifestPath string
	var originalManifest []byte
	var credentialRollbacks []func() error
	defer func() {
		if removeErr == nil {
			return
		}
		for index := len(credentialRollbacks) - 1; index >= 0; index-- {
			if err := credentialRollbacks[index](); err != nil {
				removeErr = errors.Join(removeErr, fmt.Errorf("remove: restore credential-store state: %w", err))
			}
		}
		if originalManifest != nil {
			if err := setup.WriteFileAtomically(manifestPath, originalManifest, 0o644); err != nil {
				removeErr = errors.Join(removeErr, fmt.Errorf("remove: restore package.json after failure: %w", err))
			}
		}
	}()
	if len(config.ScriptBindings) > 0 {
		manifestPath = filepath.Join(filepath.Dir(configPath), "package.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("remove: read package.json: %w", err)
		}
		originalManifest = data
		restored, err := setup.RestorePackageScripts(data, config.ScriptBindings)
		if err != nil {
			return fmt.Errorf("remove: restore package.json: %w", err)
		}
		if err := setup.WriteFileAtomically(manifestPath, restored, 0o644); err != nil {
			return fmt.Errorf("remove: write package.json: %w", err)
		}
	}
	profileNames := make([]string, 0, len(config.Profiles))
	for profileName := range config.Profiles {
		profileNames = append(profileNames, profileName)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		profile := config.Profiles[profileName]
		if profile.Provider == "local" {
			secrets, err := credentialStore.Get(config.ProjectID, profileName)
			if err == store.ErrUnavailable {
				continue
			}
			if err != nil {
				return fmt.Errorf("remove: inspect local profile %q: %w", profileName, err)
			}
			if err := credentialStore.Delete(config.ProjectID, profileName); err != nil {
				return fmt.Errorf("remove: delete local profile %q: %w", profileName, err)
			}
			name := profileName
			credentialRollbacks = append(credentialRollbacks, func() error {
				return credentialStore.Put(config.ProjectID, name, secrets)
			})
			continue
		}
		secrets, cachedAt, err := credentialStore.GetCache(config.ProjectID, profileName)
		if err == store.ErrUnavailable {
			continue
		}
		if err != nil {
			return fmt.Errorf("remove: inspect remote cache for profile %q: %w", profileName, err)
		}
		if err := credentialStore.DeleteCache(config.ProjectID, profileName); err != nil {
			return fmt.Errorf("remove: delete remote cache for profile %q: %w", profileName, err)
		}
		name := profileName
		credentialRollbacks = append(credentialRollbacks, func() error {
			return credentialStore.PutCache(config.ProjectID, name, secrets, cachedAt)
		})
	}
	if err := os.Remove(configPath); err != nil {
		return fmt.Errorf("remove: delete %s: %w", project.FileName, err)
	}
	return nil
}

func init() {
	removeCmd.Flags().BoolVar(&removeConfirm, "yes", false, "confirm project removal")
	rootCmd.AddCommand(removeCmd)
}
