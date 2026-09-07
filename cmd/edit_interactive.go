package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"
)

func promptEditProfileWithIO(profiles []editProfile, input io.Reader, output io.Writer, accessible bool) (string, error) {
	options := make([]huh.Option[string], 0, len(profiles))
	for _, profile := range profiles {
		options = append(options, huh.NewOption(fmt.Sprintf("%s (%s)", profile.Name, providerDisplayName(profile.Provider)), profile.Name))
	}
	selected := profiles[0].Name
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Profile to edit").Options(options...).Value(&selected),
	)).WithInput(input).WithOutput(output).WithAccessible(accessible)
	if err := form.Run(); err != nil {
		return "", err
	}
	return selected, nil
}

func promptSecretSetWithIO(profile string, secrets map[string]string, input io.Reader, output io.Writer, accessible bool) (map[string]string, bool, error) {
	return runSecretEditor(secrets, func(view editView) (editAction, error) {
		action, err := promptEditAction(profile, view, input, output, accessible)
		if errors.Is(err, huh.ErrUserAborted) {
			return editAction{Kind: editCancel}, nil
		}
		return action, err
	})
}

func promptEditAction(profile string, view editView, input io.Reader, output io.Writer, accessible bool) (editAction, error) {
	choice := "add"
	options := []huh.Option[string]{huh.NewOption("Add variable", "add")}
	if len(view.Entries) > 0 {
		options = append(options,
			huh.NewOption("Reveal value", "reveal"),
			huh.NewOption("Rename variable", "rename"),
			huh.NewOption("Update value", "update"),
			huh.NewOption("Delete variable", "delete"),
			huh.NewOption("Save changes", "save"),
		)
	}
	options = append(options, huh.NewOption("Cancel", "cancel"))
	form := huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Local profile: "+profile).Description(formatEditView(view)),
		huh.NewSelect[string]().Title("Action").Options(options...).Value(&choice),
	)).WithInput(input).WithOutput(output).WithAccessible(accessible)
	if err := form.Run(); err != nil {
		return editAction{}, err
	}

	switch choice {
	case "reveal", "update", "delete":
		name, err := promptExistingName(choiceTitle(choice), view, input, output, accessible)
		if err != nil {
			return editAction{}, err
		}
		switch choice {
		case "reveal":
			return editAction{Kind: editReveal, Name: name}, nil
		case "update":
			value, err := promptSecretValue("New value for "+name, input, output, accessible)
			return editAction{Kind: editUpdate, Name: name, Value: value}, err
		default:
			return editAction{Kind: editDelete, Name: name}, nil
		}
	case "rename":
		name, err := promptExistingName("Variable to rename", view, input, output, accessible)
		if err != nil {
			return editAction{}, err
		}
		newName, err := promptEnvironmentName("New variable name", view, input, output, accessible)
		return editAction{Kind: editRename, Name: name, NewName: newName}, err
	case "add":
		name, err := promptEnvironmentName("Variable name", view, input, output, accessible)
		if err != nil {
			return editAction{}, err
		}
		value, err := promptSecretValue("Value for "+name, input, output, accessible)
		return editAction{Kind: editAdd, Name: name, Value: value}, err
	case "save":
		confirmed := false
		form := huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Save the complete secret set?").Description("This performs one credential-store write").Value(&confirmed).Affirmative("Save").Negative("Keep editing"),
		)).WithInput(input).WithOutput(output).WithAccessible(accessible)
		if err := form.Run(); err != nil {
			return editAction{}, err
		}
		if confirmed {
			return editAction{Kind: editConfirm}, nil
		}
		return promptEditAction(profile, view, input, output, accessible)
	default:
		return editAction{Kind: editCancel}, nil
	}
}

func promptExistingName(title string, view editView, input io.Reader, output io.Writer, accessible bool) (string, error) {
	options := make([]huh.Option[string], 0, len(view.Entries))
	for _, entry := range view.Entries {
		options = append(options, huh.NewOption(entry.Name, entry.Name))
	}
	selected := view.Entries[0].Name
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title(title).Options(options...).Value(&selected),
	)).WithInput(input).WithOutput(output).WithAccessible(accessible)
	return selected, form.Run()
}

func promptEnvironmentName(title string, view editView, input io.Reader, output io.Writer, accessible bool) (string, error) {
	name := ""
	existing := make(map[string]string, len(view.Entries))
	for _, entry := range view.Entries {
		existing[entry.Name] = ""
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(title).Value(&name).Validate(func(value string) error {
			return validateNewEnvironmentName(value, existing)
		}),
	)).WithInput(input).WithOutput(output).WithAccessible(accessible)
	return name, form.Run()
}

func promptSecretValue(title string, input io.Reader, output io.Writer, accessible bool) (string, error) {
	value := ""
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(title).EchoMode(huh.EchoModePassword).Value(&value),
	)).WithInput(input).WithOutput(output).WithAccessible(accessible)
	return value, form.Run()
}

func formatEditView(view editView) string {
	if len(view.Entries) == 0 {
		return "No variables staged"
	}
	lines := make([]string, 0, len(view.Entries))
	for _, entry := range view.Entries {
		lines = append(lines, entry.Name+" = "+entry.Value)
	}
	return strings.Join(lines, "\n")
}

func choiceTitle(choice string) string {
	return strings.ToUpper(choice[:1]) + choice[1:] + " variable"
}
