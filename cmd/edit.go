package cmd

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/harsh-sonkar/env-pull/internal/crypto"
	"github.com/harsh-sonkar/env-pull/internal/project"
	"github.com/harsh-sonkar/env-pull/internal/store"
)

const defaultVaultFile = ".env.pull.enc"

type editProfile struct {
	Name     string
	Provider string
}

type selectEditProfile func([]editProfile) (string, error)

type editSecretSet func(string, map[string]string) (map[string]string, bool, error)

type editActionKind int

const (
	editReveal editActionKind = iota
	editAdd
	editRename
	editUpdate
	editDelete
	editConfirm
	editCancel
)

const maskedSecret = "********"

var portableEnvironmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type editAction struct {
	Kind    editActionKind
	Name    string
	NewName string
	Value   string
}

type editEntry struct {
	Name  string
	Value string
}

type editView struct {
	Entries []editEntry
}

type nextEditAction func(editView) (editAction, error)

func runEdit(loadConfig func() (project.Config, error), credentialStore store.Store, selectProfile selectEditProfile, edit editSecretSet, legacyEdit func(string) error) error {
	config, err := loadConfig()
	if errors.Is(err, os.ErrNotExist) {
		if err := legacyEdit(defaultVaultFile); err != nil {
			return fmt.Errorf("edit: legacy encrypted vault: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("edit: %w", err)
	}
	return runConfiguredEdit(config, credentialStore, selectProfile, edit)
}

func runConfiguredEdit(config project.Config, credentialStore store.Store, selectProfile selectEditProfile, edit editSecretSet) error {
	profiles := make([]editProfile, 0, len(config.Profiles))
	for name, profile := range config.Profiles {
		profiles = append(profiles, editProfile{Name: name, Provider: profile.Provider})
	}
	sort.Slice(profiles, func(left, right int) bool { return profiles[left].Name < profiles[right].Name })

	profileName, err := selectProfile(profiles)
	if err != nil {
		return err
	}
	profile, err := config.Profile(profileName)
	if err != nil {
		return fmt.Errorf("edit: %w", err)
	}
	if profile.Provider != "local" {
		return fmt.Errorf("edit: %s profiles are provider-owned; edit this secret set in %s", profileName, providerDisplayName(profile.Provider))
	}
	secrets, err := credentialStore.Get(config.ProjectID, profileName)
	if err != nil {
		return fmt.Errorf("edit: local secret set is unavailable")
	}
	edited, confirmed, err := edit(profileName, secrets)
	if err != nil {
		return fmt.Errorf("edit: %w", err)
	}
	if !confirmed {
		return nil
	}
	if err := credentialStore.Put(config.ProjectID, profileName, edited); err != nil {
		return fmt.Errorf("edit: save local secret set: %w", err)
	}
	return nil
}

func runSecretEditor(initial map[string]string, next nextEditAction) (map[string]string, bool, error) {
	secrets := make(map[string]string, len(initial))
	for name, value := range initial {
		secrets[name] = value
	}
	revealed := make(map[string]bool)

	for {
		action, err := next(secretEditView(secrets, revealed))
		if err != nil {
			return nil, false, err
		}
		switch action.Kind {
		case editReveal:
			if _, exists := secrets[action.Name]; !exists {
				return nil, false, fmt.Errorf("unknown environment-variable name %q", action.Name)
			}
			revealed[action.Name] = true
		case editAdd:
			if err := validateNewEnvironmentName(action.Name, secrets); err != nil {
				return nil, false, err
			}
			secrets[action.Name] = action.Value
		case editRename:
			value, exists := secrets[action.Name]
			if !exists {
				return nil, false, fmt.Errorf("unknown environment-variable name %q", action.Name)
			}
			if action.NewName != action.Name {
				if err := validateNewEnvironmentName(action.NewName, secrets); err != nil {
					return nil, false, err
				}
				delete(secrets, action.Name)
				delete(revealed, action.Name)
				secrets[action.NewName] = value
			}
		case editUpdate:
			if _, exists := secrets[action.Name]; !exists {
				return nil, false, fmt.Errorf("unknown environment-variable name %q", action.Name)
			}
			secrets[action.Name] = action.Value
		case editDelete:
			if _, exists := secrets[action.Name]; !exists {
				return nil, false, fmt.Errorf("unknown environment-variable name %q", action.Name)
			}
			delete(secrets, action.Name)
			delete(revealed, action.Name)
		case editConfirm:
			if len(secrets) == 0 {
				return nil, false, fmt.Errorf("secret set must contain at least one environment variable")
			}
			return secrets, true, nil
		case editCancel:
			return nil, false, nil
		default:
			return nil, false, fmt.Errorf("unknown editor action")
		}
	}
}

func secretEditView(secrets map[string]string, revealed map[string]bool) editView {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	view := editView{Entries: make([]editEntry, 0, len(names))}
	for _, name := range names {
		value := maskedSecret
		if revealed[name] {
			value = secrets[name]
		}
		view.Entries = append(view.Entries, editEntry{Name: name, Value: value})
	}
	return view
}

func validateNewEnvironmentName(name string, secrets map[string]string) error {
	if !portableEnvironmentName.MatchString(name) {
		return fmt.Errorf("invalid environment-variable name %q", name)
	}
	if _, exists := secrets[name]; exists {
		return fmt.Errorf("environment-variable name %q already exists", name)
	}
	return nil
}

func providerDisplayName(provider string) string {
	switch provider {
	case "1password":
		return "1Password"
	case "bitwarden":
		return "Bitwarden"
	default:
		return strings.TrimSpace(provider)
	}
}

var editCmd = &cobra.Command{
	Use:   "edit",
	Short: "Edit a configured local secret set in the terminal",
	Long: `edit stages changes to one configured local profile entirely in memory,
then writes the complete secret set once after confirmation. Remote profiles
must be edited in their owning provider.

When inject.toml is absent, edit uses the legacy encrypted-vault editor for
backward compatibility. Configured projects never open $EDITOR or create a
plaintext temporary file.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		input := cmd.InOrStdin()
		output := cmd.OutOrStdout()
		return runEdit(project.Find, newCredentialStore(),
			func(profiles []editProfile) (string, error) {
				return promptEditProfileWithIO(profiles, input, output, false)
			},
			func(profile string, secrets map[string]string) (map[string]string, bool, error) {
				return promptSecretSetWithIO(profile, secrets, input, output, false)
			},
			crypto.OpenInEditor,
		)
	},
}

func init() {
	rootCmd.AddCommand(editCmd)
}
