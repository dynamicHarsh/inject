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
			huh.NewMultiSelect[string]().Title("Developer commands").Description("Likely runtime commands are preselected").Options(commandOptions...).Value(&selectedCommands),
		).Title("Commands").WithHide(len(commandOptions) == 0),
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
				return setupReview(projectID, source, selectedInputs, selectedCommands, validationChoice, customValidation, discovery.PackageManager, removePlaintext)
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
	request.PackageScripts = selectedCommands
	request.Validate = selectedValidation(validationChoice, customValidation, discovery.PackageManager)
	request.Confirm = confirmed
	request.RemoveLegacyEnv = removePlaintext
	request.ConfirmRemoveEnv = removePlaintext
	request.NonInteractive = false
	fmt.Fprintln(output, "Applying setup...")
	return request, nil
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
