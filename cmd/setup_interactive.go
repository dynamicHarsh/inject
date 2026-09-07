package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"

	setupworkflow "github.com/harsh-sonkar/env-pull/internal/setup"
)

var errSetupCancelled = errors.New("setup cancelled")

func parsePackageScriptCommand(command string) (string, string, error) {
	if strings.ContainsAny(command, "|&;<>") {
		return "", "", fmt.Errorf("unsupported developer command %q: shell expressions are not supported", command)
	}
	fields := strings.Fields(command)
	if len(fields) == 3 && fields[1] == "run" {
		switch fields[0] {
		case "npm", "pnpm", "yarn", "bun":
			return fields[0], fields[2], nil
		}
	}
	if len(fields) == 2 {
		switch fields[0] {
		case "pnpm", "yarn":
			return fields[0], fields[1], nil
		}
	}
	return "", "", fmt.Errorf("unsupported developer command %q: enter an npm, pnpm, Yarn, or Bun package-script invocation", command)
}

func runGuidedSetup(
	request setupworkflow.Request,
	discover func(string) (setupworkflow.Discovery, error),
	prompt func(setupworkflow.Discovery, setupworkflow.Request) (setupworkflow.Request, error),
	run func(setupworkflow.Request) error,
) error {
	discovery, err := discover(request.Directory)
	if err != nil {
		return err
	}
	request, err = prompt(discovery, request)
	if errors.Is(err, errSetupCancelled) {
		return nil
	}
	if err != nil {
		return err
	}
	return run(request)
}

func promptSetup(discovery setupworkflow.Discovery, request setupworkflow.Request) (setupworkflow.Request, error) {
	return promptSetupWithIO(discovery, request, setupInput, request.Output, false)
}

func promptSetupWithIO(discovery setupworkflow.Discovery, request setupworkflow.Request, input io.Reader, output io.Writer, accessible bool) (setupworkflow.Request, error) {
	projectID := discovery.ProjectID
	if request.ProjectID != "" {
		projectID = request.ProjectID
	}
	source := discovery.DefaultSource
	if request.Local {
		source = "local"
	} else if request.Provider != "" {
		source = request.Provider
	}
	selectedInputs := append([]string(nil), discovery.SelectedInputs...)
	if request.SelectedInputs != nil {
		selectedInputs = append([]string(nil), request.SelectedInputs...)
	}
	selectedCommands := append([]string(nil), discovery.SelectedCommands...)
	if request.PackageScripts != nil {
		selectedCommands = append([]string(nil), request.PackageScripts...)
	}
	primaryScript := ""
	if len(selectedCommands) > 0 {
		primaryScript = selectedCommands[0]
	} else {
		for _, command := range discovery.DeveloperCommands {
			if command.Default {
				primaryScript = command.Name
				break
			}
		}
	}
	primaryCommand := formatPackageScriptCommand(discovery.PackageManager, primaryScript)
	additionalCommands := withoutCommand(selectedCommands, primaryScript)
	validationChoice := ""
	customValidation := strings.Join(request.Validate, " ")
	if len(request.Validate) > 0 {
		validationChoice = "custom"
	} else if len(discovery.ValidationCandidates) > 0 {
		validationChoice = discovery.ValidationCandidates[0]
	}
	removePlaintext := request.RemoveLegacyEnv
	confirmed := false

	inputOptions := make([]huh.Option[string], 0, len(discovery.PlaintextInputs))
	for _, input := range discovery.PlaintextInputs {
		inputOptions = append(inputOptions, huh.NewOption(input.Name+" -> profile "+input.Profile, input.Name))
	}
	commandOptions := make([]huh.Option[string], 0, len(discovery.DeveloperCommands))
	for _, command := range discovery.DeveloperCommands {
		commandOptions = append(commandOptions, huh.NewOption(command.Name, command.Name))
	}
	validationOptions := []huh.Option[string]{huh.NewOption("No validation", "")}
	for _, name := range discovery.ValidationCandidates {
		validationOptions = append(validationOptions, huh.NewOption(formatValidation(discovery.PackageManager, name), name))
	}
	validationOptions = append(validationOptions, huh.NewOption("Enter a custom finite command", "custom"))

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("Project ID").Value(&projectID).Validate(nonEmpty("project ID")),
			huh.NewSelect[string]().Title("Secret source").Options(
				huh.NewOption("Local credential store", "local"),
				huh.NewOption("1Password", "1password"),
				huh.NewOption("Bitwarden", "bitwarden"),
			).Value(&source),
		).Title("Project"),
		huh.NewGroup(
			huh.NewInput().Title("1Password account").Value(&request.Account).Validate(nonEmpty("account")),
			huh.NewInput().Title("1Password vault").Value(&request.Vault).Validate(nonEmpty("vault")),
			huh.NewInput().Title("Item ID").Description("Preferred when available").Value(&request.ItemID),
			huh.NewInput().Title("Item name").Description("Used only when no item ID is set").Value(&request.Item).Validate(remoteReference(&request.ItemID)),
		).Title("1Password source").WithHideFunc(func() bool { return source != "1password" }),
		huh.NewGroup(
			huh.NewInput().Title("Item ID").Description("Preferred when available").Value(&request.ItemID),
			huh.NewInput().Title("Item name").Description("Used only when no item ID is set").Value(&request.Item).Validate(remoteReference(&request.ItemID)),
		).Title("Bitwarden source").WithHideFunc(func() bool { return source != "bitwarden" }),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Environment inputs and profiles").Description("Space toggles a selection").Options(inputOptions...).Value(&selectedInputs).
				Validate(func(values []string) error {
					if source == "local" && len(values) == 0 {
						return fmt.Errorf("select at least one input for Local")
					}
					return nil
				}),
		).Title("Profiles").WithHide(len(inputOptions) == 0),
		huh.NewGroup(
			huh.NewInput().Title("Primary developer command").Description("Confirm or enter a package-script command").Value(&primaryCommand).
				Validate(func(value string) error { return validatePrimaryCommand(value, discovery) }),
		).Title("Primary command"),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Additional developer commands").Description("Space toggles a selection").Options(commandOptions...).Value(&additionalCommands),
		).Title("Additional commands").WithHide(len(commandOptions) == 0),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Finite validation command").Options(validationOptions...).Value(&validationChoice),
		).Title("Validation"),
		huh.NewGroup(
			huh.NewInput().Title("Custom validation command").Value(&customValidation).Validate(nonEmpty("validation command")),
		).Title("Validation").WithHideFunc(func() bool { return validationChoice != "custom" }),
		huh.NewGroup(
			huh.NewConfirm().Title("Remove .env after successful validation?").Description("The default keeps plaintext files unchanged").Value(&removePlaintext).Affirmative("Remove").Negative("Keep"),
		).Title("Cleanup").WithHide(!hasInput(discovery.PlaintextInputs, ".env")),
		huh.NewGroup(
			huh.NewNote().Title("Review setup").DescriptionFunc(func() string {
				_, currentPrimary, _ := parsePackageScriptCommand(primaryCommand)
				commands := append([]string{currentPrimary}, withoutCommand(additionalCommands, currentPrimary)...)
				return setupReview(projectID, source, selectedInputs, commands, validationChoice, customValidation, discovery.PackageManager, removePlaintext)
			}, nil),
			huh.NewConfirm().Title("Apply these changes?").Value(&confirmed).Affirmative("Apply").Negative("Cancel"),
		).Title("Review"),
	).WithInput(input).WithOutput(output).WithAccessible(accessible)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, errSetupCancelled) {
			return setupworkflow.Request{}, errSetupCancelled
		}
		return setupworkflow.Request{}, fmt.Errorf("setup: interactive flow: %w", err)
	}
	if !confirmed {
		return setupworkflow.Request{}, errSetupCancelled
	}

	request.ProjectID = projectID
	request.Provider = source
	request.Local = source == "local"
	if request.Local {
		request.Provider = ""
	}
	request.SelectedInputs = selectedInputs
	_, primaryScript, _ = parsePackageScriptCommand(primaryCommand)
	request.PackageScripts = append([]string{primaryScript}, withoutCommand(additionalCommands, primaryScript)...)
	request.Validate = selectedValidation(validationChoice, customValidation, discovery.PackageManager)
	request.Confirm = confirmed
	request.RemoveLegacyEnv = removePlaintext
	request.ConfirmRemoveEnv = removePlaintext
	request.NonInteractive = false
	request.ResolveConflict = func(conflict setupworkflow.Conflict) (setupworkflow.ConflictResolution, error) {
		return promptSetupConflict(conflict, input, output, accessible)
	}
	fmt.Fprintln(output, "Applying setup...")
	return request, nil
}

func promptSetupConflict(conflict setupworkflow.Conflict, input io.Reader, output io.Writer, accessible bool) (setupworkflow.ConflictResolution, error) {
	resolution := setupworkflow.RetainConflict
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[setupworkflow.ConflictResolution]().
			Title(fmt.Sprintf("Owned package script %q was changed", conflict.Script)).
			Description("Retain the current entry or explicitly restore inject's recorded value").
			Options(
				huh.NewOption("Retain current entry", setupworkflow.RetainConflict),
				huh.NewOption("Replace with recorded value", setupworkflow.ReplaceConflict),
			).
			Value(&resolution),
	)).WithInput(input).WithOutput(output).WithAccessible(accessible)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return setupworkflow.RetainConflict, errSetupCancelled
		}
		return setupworkflow.RetainConflict, fmt.Errorf("setup: resolve package script conflict: %w", err)
	}
	return resolution, nil
}

func nonEmpty(name string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
}

func remoteReference(itemID *string) func(string) error {
	return func(item string) error {
		if strings.TrimSpace(*itemID) == "" && strings.TrimSpace(item) == "" {
			return fmt.Errorf("item ID or name is required")
		}
		return nil
	}
}

func selectedValidation(choice, custom, manager string) []string {
	if choice == "custom" {
		return strings.Fields(custom)
	}
	if choice == "" {
		return nil
	}
	if manager == "" {
		return []string{choice}
	}
	return []string{manager, "run", choice}
}

func formatValidation(manager, name string) string {
	if manager == "" {
		return name
	}
	return manager + " run " + name
}

func formatPackageScriptCommand(manager, script string) string {
	if manager == "" || script == "" {
		return ""
	}
	if manager == "yarn" {
		return manager + " " + script
	}
	return manager + " run " + script
}

func validatePrimaryCommand(command string, discovery setupworkflow.Discovery) error {
	manager, script, err := parsePackageScriptCommand(command)
	if err != nil {
		return err
	}
	if manager != discovery.PackageManager {
		return fmt.Errorf("developer command uses %s, but this package uses %s", manager, discovery.PackageManager)
	}
	for _, candidate := range discovery.DeveloperCommands {
		if candidate.Name == script {
			return nil
		}
	}
	return fmt.Errorf("package.json has no %q script", script)
}

func withoutCommand(commands []string, excluded string) []string {
	filtered := make([]string, 0, len(commands))
	seen := make(map[string]bool, len(commands))
	for _, command := range commands {
		if command == excluded || seen[command] {
			continue
		}
		seen[command] = true
		filtered = append(filtered, command)
	}
	return filtered
}

func hasInput(inputs []setupworkflow.PlaintextInput, name string) bool {
	for _, input := range inputs {
		if input.Name == name {
			return true
		}
	}
	return false
}

func setupReview(projectID, source string, inputs, commands []string, validationChoice, customValidation, manager string, removePlaintext bool) string {
	validation := strings.Join(selectedValidation(validationChoice, customValidation, manager), " ")
	if validation == "" {
		validation = "none"
	}
	cleanup := "keep plaintext inputs"
	if removePlaintext {
		cleanup = "remove .env after validation"
	}
	return fmt.Sprintf("Project ID: %s\nSource: %s\nProfiles: %s\nDeveloper commands: %s\nValidation: %s\nCleanup: %s",
		projectID, source, listOrNone(inputs), listOrNone(commands), validation, cleanup)
}

func listOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
