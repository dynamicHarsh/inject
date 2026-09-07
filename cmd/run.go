package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/harsh-sonkar/env-pull/internal/executor"
	"github.com/harsh-sonkar/env-pull/internal/project"
	"github.com/harsh-sonkar/env-pull/internal/store"
	"github.com/harsh-sonkar/env-pull/internal/vaults"
)

var newCredentialStore = func() store.Store { return store.NewSystem() }

// runCmd is intentionally thin: it resolves secrets from the appropriate
// backend, then delegates all execution to executor.RunCommand.
var runCmd = &cobra.Command{
	Use:   "run [--profile <name>] [--offline] -- <command> [args...]",
	Short: "Run a command with configured secrets injected into its environment",
	Long: `run is the core command of inject. It resolves secrets from inject.toml,
then spawns your target command as a transparent child process with those
secrets present in its environment. The child sees them as ordinary env vars;
they vanish when it exits. Nothing is written to disk.

The default profile is selected unless --profile names another configured remote
secret note. inject reuses the existing provider CLI session; run op signin or
bw login before retrying when it reports an unavailable session.

USAGE EXAMPLES

	inject run -- ./server

	# Run with a named profile
	inject run --profile staging -- psql --host=localhost mydb

  # Works with any program, including shell built-ins via a shell wrapper
	inject run -- printenv DB_PASSWORD

NOTE: all inject flags must appear before the child command. Arguments
after the child binary name are passed through unchanged, including flags
that look like -x or --flag (flag parsing is disabled for this subcommand).`,

	// DisableFlagParsing passes all arguments through to the child command
	// unchanged. Our own flags are extracted manually before dispatch.
	DisableFlagParsing: true,

	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("run: a command to execute is required")
		}

		profileName, offline, childArgs := extractRunFlags(args)
		if len(childArgs) > 0 && childArgs[0] == "--" {
			childArgs = childArgs[1:]
		}
		if len(childArgs) == 0 {
			return fmt.Errorf("run: a command to execute is required after --profile")
		}

		secrets, err := loadProfileSecrets(profileName, offline)
		if err != nil {
			return err
		}

		return runChild(childArgs, secrets)
	},
}

func runChild(command []string, secrets map[string]string) error {
	if err := executor.RunCommand(command, secrets); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("run: %w", err)
	}
	return nil
}

// extractRunFlags scans args for inject's flags before passing the rest to the child.
func extractRunFlags(args []string) (profileName string, offline bool, remaining []string) {
	remaining = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--profile" && i+1 < len(args) {
			profileName = args[i+1]
			i++ // consume the value token
			continue
		}
		if after, found := strings.CutPrefix(args[i], "--profile="); found {
			profileName = after
			continue
		}
		if args[i] == "--offline" {
			offline = true
			continue
		}
		remaining = append(remaining, args[i])
	}
	return
}

func loadProfileSecrets(profileName string, offline bool) (map[string]string, error) {
	config, err := project.Find()
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	profile, err := config.Profile(profileName)
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	switch profile.Provider {
	case "local":
		return loadLocalProfileSecrets(config, profileName, newCredentialStore())
	case "1password":
		return loadRemoteProfileSecrets(config, profileName, offline, newCredentialStore(), isCI(), time.Now(), func() (map[string]string, error) {
			provider, err := vaults.NewOnePasswordProvider()
			if err != nil {
				return nil, fmt.Errorf("run: %w", err)
			}
			return provider.Fetch(context.Background(), vaults.OnePasswordReference{
				Account: profile.Account,
				Vault:   profile.Vault,
				ItemID:  profile.ItemID,
				Item:    profile.Item,
			})
		})
	case "bitwarden":
		return loadRemoteProfileSecrets(config, profileName, offline, newCredentialStore(), isCI(), time.Now(), func() (map[string]string, error) {
			provider, err := vaults.NewBitwardenProvider()
			if err != nil {
				return nil, fmt.Errorf("run: %w", err)
			}
			return provider.Fetch(context.Background(), vaults.BitwardenReference{
				ItemID: profile.ItemID,
				Item:   profile.Item,
			})
		})
	default:
		return nil, fmt.Errorf("run: unsupported provider %q", profile.Provider)
	}
}

func loadRemoteProfileSecrets(config project.Config, profileName string, offline bool, credentialStore store.Store, ci bool, now time.Time, fetch func() (map[string]string, error)) (map[string]string, error) {
	if profileName == "" {
		profileName = "default"
	}
	if offline {
		if ci || !config.Cache.Enabled {
			return nil, fmt.Errorf("run: offline remote cache is unavailable")
		}
		secrets, cachedAt, err := credentialStore.GetCache(config.ProjectID, profileName)
		cacheAge := now.Sub(cachedAt)
		if err != nil || cacheAge < 0 || cacheAge >= config.Cache.MaxAge.Duration {
			return nil, fmt.Errorf("run: offline remote cache is unavailable")
		}
		return secrets, nil
	}

	secrets, err := fetch()
	if err != nil {
		return nil, err
	}
	if config.Cache.Enabled && !ci {
		if err := credentialStore.PutCache(config.ProjectID, profileName, secrets, now); err != nil {
			return nil, fmt.Errorf("run: remote cache is unavailable")
		}
	}
	return secrets, nil
}

func isCI() bool {
	_, present := os.LookupEnv("CI")
	return present
}

func loadLocalProfileSecrets(config project.Config, profileName string, credentialStore store.Store) (map[string]string, error) {
	if profileName == "" {
		profileName = "default"
	}
	secrets, err := credentialStore.Get(config.ProjectID, profileName)
	if err != nil {
		return nil, fmt.Errorf("run: local secret set is unavailable; add an uncommitted .env and run `inject setup --local` to provision this machine")
	}
	return secrets, nil
}

func init() {
	rootCmd.AddCommand(runCmd)
}
