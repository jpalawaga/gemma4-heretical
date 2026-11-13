package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The memory category editor is reached from the settings screen. It edits a
// draft list of "name: description" rows and writes the categories file on
// save, so the agent's save_memory enum and ./memory pick the change up at
// once. Memories already filed under a removed category keep it; they show
// up as "in use but not configured" until moved.

func (m *appModel) openCategoryEditor() {
	categories, err := m.memory.Categories()
	m.categoriesDraft = append([]memoryCategory(nil), categories...)
	m.categoriesCursor = 0
	m.categoriesEditing = false
	m.categoriesAdding = false
	m.categoriesError = ""
	if err != nil {
		m.categoriesError = "could not read " + m.draftCategoriesPath() + ": " + err.Error()
	}
	m.categoriesCounts = map[string]int{}
	if entries, err := m.memory.Load(); err == nil {
		for _, entry := range entries {
			m.categoriesCounts[entry.Category]++
		}
	}
	m.phase = phaseCategoryEditor
	m.status = "Memory categories · a add · Enter edit · d delete · s save · esc back"
}

// draftCategoriesPath resolves the categories file the editor writes, using
// the unsaved settings draft so a just-changed path is honoured.
func (m appModel) draftCategoriesPath() string {
	return resolveCategoriesPath(m.memory.dir, m.settingsDraft.MemoryCategoriesFile)
}

func (m *appModel) startCategoryEdit(adding bool) tea.Cmd {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = "name: description"
	input.CharLimit = 200
	input.SetWidth(max(20, min(m.width-30, 72)))
	if !adding && m.categoriesCursor < len(m.categoriesDraft) {
		category := m.categoriesDraft[m.categoriesCursor]
		value := category.Name
		if category.Description != "" {
			value += ": " + category.Description
		}
		input.SetValue(value)
		input.SetCursor(len(value))
	}
	m.categoriesInput = input
	m.categoriesEditing = true
	m.categoriesAdding = adding
	m.categoriesError = ""
	return m.categoriesInput.Focus()
}

func (m *appModel) commitCategoryEdit() {
	parsed := parseMemoryCategories(m.categoriesInput.Value())
	if len(parsed) != 1 {
		m.categoriesError = "enter one category as \"name: description\""
		return
	}
	category := parsed[0]
	for index, existing := range m.categoriesDraft {
		if existing.Name == category.Name && (m.categoriesAdding || index != m.categoriesCursor) {
			m.categoriesError = "a category named " + category.Name + " already exists"
			return
		}
	}
	if m.categoriesAdding {
		m.categoriesDraft = append(m.categoriesDraft, category)
		m.categoriesCursor = len(m.categoriesDraft) - 1
	} else {
		m.categoriesDraft[m.categoriesCursor] = category
	}
	m.categoriesEditing = false
	m.categoriesAdding = false
	m.categoriesError = ""
}

func (m *appModel) deleteCategory() {
	if m.categoriesCursor >= len(m.categoriesDraft) {
		return
	}
	m.categoriesDraft = append(m.categoriesDraft[:m.categoriesCursor], m.categoriesDraft[m.categoriesCursor+1:]...)
	if m.categoriesCursor >= len(m.categoriesDraft) && m.categoriesCursor > 0 {
		m.categoriesCursor--
	}
	m.categoriesError = ""
}

func (m *appModel) moveCategory(step int) {
	target := m.categoriesCursor + step
	if m.categoriesCursor >= len(m.categoriesDraft) || target < 0 || target >= len(m.categoriesDraft) {
		return
	}
	m.categoriesDraft[m.categoriesCursor], m.categoriesDraft[target] = m.categoriesDraft[target], m.categoriesDraft[m.categoriesCursor]
	m.categoriesCursor = target
}

func (m *appModel) saveCategories() bool {
	path := m.draftCategoriesPath()
	if err := writeMemoryCategories(path, m.categoriesDraft); err != nil {
		m.categoriesError = "could not write " + path + ": " + err.Error()
		return false
	}
	m.phase = phaseSettings
	m.status = fmt.Sprintf("Saved %d memory categor%s to %s", len(m.categoriesDraft), pluralSuffix(len(m.categoriesDraft), "y", "ies"), path)
	return true
}

// writeMemoryCategories stores the list in the "name: description" format
// that chat2 and ./memory read. An empty list removes the file so categories
// become free-form again.
func writeMemoryCategories(path string, categories []memoryCategory) error {
	if len(categories) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var content strings.Builder
	content.WriteString("# Memory categories for chat2, one per line as \"name: description\".\n")
	content.WriteString("# Edited in the app with F2 › Memory categories on " + time.Now().Format("2006-01-02") + ".\n")
	for _, category := range categories {
		content.WriteString(category.Name)
		if category.Description != "" {
			content.WriteString(": " + category.Description)
		}
		content.WriteString("\n")
	}
	return os.WriteFile(path, []byte(content.String()), 0o600)
}

func (m appModel) updateCategoryEditor(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		if m.categoriesEditing {
			var cmd tea.Cmd
			m.categoriesInput, cmd = m.categoriesInput.Update(msg)
			commands = append(commands, cmd)
		}
		return m, tea.Batch(commands...)
	}
	if m.categoriesEditing {
		switch key.String() {
		case "enter":
			m.commitCategoryEdit()
		case "esc":
			m.categoriesEditing = false
			m.categoriesAdding = false
			m.categoriesError = ""
		case "ctrl+c":
			return m, tea.Quit
		default:
			var cmd tea.Cmd
			m.categoriesInput, cmd = m.categoriesInput.Update(msg)
			commands = append(commands, cmd)
		}
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		m.phase = phaseSettings
		m.status = "Memory categories unchanged"
	case "up", "k":
		if m.categoriesCursor > 0 {
			m.categoriesCursor--
		}
	case "down", "j":
		if m.categoriesCursor < len(m.categoriesDraft)-1 {
			m.categoriesCursor++
		}
	case "shift+up", "K":
		m.moveCategory(-1)
	case "shift+down", "J":
		m.moveCategory(1)
	case "enter", "e":
		if len(m.categoriesDraft) > 0 {
			commands = append(commands, m.startCategoryEdit(false))
		} else {
			commands = append(commands, m.startCategoryEdit(true))
		}
	case "a", "n":
		commands = append(commands, m.startCategoryEdit(true))
	case "d", "x", "delete", "backspace":
		m.deleteCategory()
	case "s", "ctrl+s":
		m.saveCategories()
	}
	return m, tea.Batch(commands...)
}

func (m appModel) renderCategoryEditor() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Memory categories")
	subtitle := lipgloss.NewStyle().Foreground(m.palette.muted).Render(m.draftCategoriesPath())
	nameWidth := 4
	for _, category := range m.categoriesDraft {
		nameWidth = max(nameWidth, lipgloss.Width(category.Name))
	}
	var rows []string
	for index, category := range m.categoriesDraft {
		selected := index == m.categoriesCursor
		cursor := "  "
		nameStyle := lipgloss.NewStyle().Bold(true).Foreground(m.palette.text)
		if selected {
			cursor = "› "
			nameStyle = nameStyle.Foreground(m.palette.secondary)
		}
		if selected && m.categoriesEditing && !m.categoriesAdding {
			rows = append(rows, cursor+m.categoriesInput.View())
			continue
		}
		count := lipgloss.NewStyle().Foreground(m.palette.muted).Render(fmt.Sprintf("  (%d)", m.categoriesCounts[category.Name]))
		description := lipgloss.NewStyle().Foreground(m.palette.text).Render(category.Description)
		rows = append(rows, cursor+nameStyle.Render(fmt.Sprintf("%-*s", nameWidth, category.Name))+count+"  "+description)
	}
	if m.categoriesEditing && m.categoriesAdding {
		rows = append(rows, "+ "+m.categoriesInput.View())
	}
	if len(rows) == 0 {
		rows = append(rows, lipgloss.NewStyle().Foreground(m.palette.muted).Render("No categories configured: names are free-form. Press a to add one."))
	}
	var unconfigured []string
	configured := map[string]bool{}
	for _, category := range m.categoriesDraft {
		configured[category.Name] = true
	}
	for name, count := range m.categoriesCounts {
		if !configured[name] {
			unconfigured = append(unconfigured, fmt.Sprintf("%s (%d)", name, count))
		}
	}
	extra := ""
	if len(unconfigured) > 0 {
		extra = "\n\n" + lipgloss.NewStyle().Foreground(m.palette.accent).Width(max(30, min(m.width-16, 84))).
			Render("In use but not configured: "+strings.Join(unconfigured, ", ")+". Add them here or re-file those memories with /memory or move_memory.")
	}
	footer := "↑/↓ move  ·  Enter edit  ·  a add  ·  d delete  ·  Shift+↑/↓ reorder  ·  s save  ·  esc back"
	if m.categoriesEditing {
		footer = "Type name: description  ·  Enter apply  ·  esc cancel"
	}
	body := title + "\n" + subtitle + "\n\n" + strings.Join(rows, "\n") + extra
	if m.categoriesError != "" {
		body += "\n\n" + lipgloss.NewStyle().Foreground(m.palette.danger).Render("✗ "+m.categoriesError)
	}
	body += "\n\n" + lipgloss.NewStyle().Foreground(m.palette.muted).Render(footer)
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.primary).
		Padding(1, 2).
		Width(max(44, min(m.width-8, 120))).
		Render(body)
	return lipgloss.NewStyle().Padding(1, 3).Render(panel)
}
