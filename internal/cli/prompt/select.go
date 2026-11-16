package prompt

import (
	"github.com/manifoldco/promptui"
	"io"
)

// SelectOption represents an item in a selection list.
type SelectOption struct {
	Label       string
	Value       string
	Description string
}

// selectTemplates returns the standard templates for selection prompts.
func selectTemplates() *promptui.SelectTemplates {
	return &promptui.SelectTemplates{
		Label:    "{{ . }}",
		Active:   "> {{ .Label | cyan }}",
		Inactive: "  {{ .Label | white }}",
		Selected: "* {{ .Label | green }}",
	}
}

// Select prompts the user to select from a list of options.
// Returns the selected option's value.
func Select(in io.Reader, label string, options []SelectOption) (string, error) {
	templates := selectTemplates()

	if len(options) > 0 && options[0].Description != "" {
		templates.Details = `
{{ "Description:" | faint }}	{{ .Description }}`
	}

	prompt := promptui.Select{
		Stdin:     io.NopCloser(in),
		Label:     label,
		Items:     options,
		Templates: templates,
		Size:      10,
	}

	i, _, err := prompt.Run()
	if err != nil {
		return "", wrapError(err)
	}

	return options[i].Value, nil
}

// SelectString prompts the user to select from a list of strings.
// Returns the selected string.
func SelectString(in io.Reader, label string, items []string) (string, error) {
	prompt := promptui.Select{
		Stdin: io.NopCloser(in),
		Label: label,
		Items: items,
		Size:  10,
	}

	_, result, err := prompt.Run()
	return result, wrapError(err)
}

// SelectIndex prompts the user to select from a list of options.
// Returns the index of the selected option.
func SelectIndex(in io.Reader, label string, options []SelectOption) (int, error) {
	prompt := promptui.Select{
		Stdin:     io.NopCloser(in),
		Label:     label,
		Items:     options,
		Templates: selectTemplates(),
		Size:      10,
	}

	i, _, err := prompt.Run()
	return i, wrapError(err)
}

// MultiSelect allows selecting multiple options (using space to toggle).
// Note: promptui doesn't natively support multi-select, so this is a workaround
// using multiple single selects with a "Done" option.
func MultiSelect(in io.Reader, label string, options []SelectOption) ([]string, error) {
	selected := make(map[string]bool)
	var result []string

	for {
		// Build items list with selection status
		items := make([]string, 0, len(options)+1)
		for _, opt := range options {
			prefix := "[ ]"
			if selected[opt.Value] {
				prefix = "[x]"
			}
			items = append(items, prefix+" "+opt.Label)
		}
		items = append(items, "Done")

		prompt := promptui.Select{
			Stdin: io.NopCloser(in),
			Label: label,
			Items: items,
			Size:  len(items),
		}

		i, _, err := prompt.Run()
		if err != nil {
			return nil, err
		}

		// "Done" selected
		if i == len(options) {
			break
		}

		// Toggle selection
		opt := options[i]
		if selected[opt.Value] {
			delete(selected, opt.Value)
		} else {
			selected[opt.Value] = true
		}
	}

	// Build result slice
	for _, opt := range options {
		if selected[opt.Value] {
			result = append(result, opt.Value)
		}
	}

	return result, nil
}
