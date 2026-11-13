package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The settings screen edits a draft copy of the persistent settings. Rows are
// typed: toggles flip in place, enums cycle with ←/→, and text, number, and
// duration rows open an inline text input. Saving writes settings.json and
// applies the values to the running session.

type settingKind int

const (
	settingText settingKind = iota
	settingBool
	settingEnum
	settingInt
	settingDuration
	settingMultiline
	settingAction
)

type settingOption struct {
	Value string
	Label string
}

type settingField struct {
	Key     string
	Label   string
	Help    string
	Kind    settingKind
	Options []settingOption
	Get     func(*settings) string
	Set     func(*settings, string) error
	Live    bool // applies to the running session immediately on save
	Action  func(*appModel)
}

func settingsFields(choices []modelChoice) []settingField {
	modelOptions := []settingOption{{Value: "", Label: "Ask at startup"}}
	for _, choice := range choices {
		label := modelKey(choice)
		if !choice.Online {
			label += " (offline)"
		}
		modelOptions = append(modelOptions, settingOption{Value: modelKey(choice), Label: label})
	}
	str := func(get func(*settings) *string) (func(*settings) string, func(*settings, string) error) {
		return func(s *settings) string { return *get(s) },
			func(s *settings, value string) error { *get(s) = strings.TrimSpace(value); return nil }
	}
	boolean := func(get func(*settings) *bool) (func(*settings) string, func(*settings, string) error) {
		return func(s *settings) string { return strconv.FormatBool(*get(s)) },
			func(s *settings, value string) error { *get(s) = value == "true"; return nil }
	}
	duration := func(get func(*settings) *string, positive bool) (func(*settings) string, func(*settings, string) error) {
		return func(s *settings) string { return *get(s) },
			func(s *settings, value string) error {
				d, err := parseNonNegativeDuration(value)
				if err != nil {
					return err
				}
				if positive && d <= 0 {
					return fmt.Errorf("must be a positive duration such as 10m")
				}
				if d == 0 {
					*get(s) = "0"
				} else {
					*get(s) = d.String()
				}
				return nil
			}
	}

	var fields []settingField
	add := func(field settingField) { fields = append(fields, field) }

	get, set := str(func(s *settings) *string { return &s.PreferredModel })
	add(settingField{Key: "preferred_model", Label: "Preferred model", Kind: settingEnum, Options: modelOptions, Get: get, Set: set,
		Help: "Connect to this model automatically when it is online instead of showing the picker."})
	get, set = str(func(s *settings) *string { return &s.SystemFile })
	add(settingField{Key: "system_file", Label: "System prompt file", Kind: settingText, Get: get, Set: set,
		Help: "Path of the file sent as the first system prompt, such as memory.md. Applies at the next model connection."})
	add(settingField{Key: "memory_categories", Label: "Memory categories", Kind: settingAction, Action: (*appModel).openCategoryEditor,
		Get: func(*settings) string { return "" }, Set: func(*settings, string) error { return nil },
		Help: "Open the category editor: add, rename, describe, reorder, or remove the categories the agent may file memories under."})
	get, set = str(func(s *settings) *string { return &s.MemoryCategoriesFile })
	add(settingField{Key: "memory_categories_file", Label: "Memory categories file", Kind: settingText, Live: true, Get: get, Set: set,
		Help: "Path of the 'name: description' category list. Leave empty for memory-categories.txt in ~/.gemma4-heretical. Relative paths resolve from the current directory."})
	add(settingField{Key: "context", Label: "Context window", Kind: settingInt, Live: true,
		Get: func(s *settings) string { return strconv.Itoa(s.Context) },
		Set: func(s *settings, value string) error {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || parsed <= 0 {
				return fmt.Errorf("must be a positive number of tokens")
			}
			s.Context = parsed
			return nil
		},
		Help: "Tokens. Sent to Ollama as num_ctx and used for the header meter; llama.cpp uses its own --ctx-size, so match GEMMA4_CONTEXT for serve-llama."})
	get, set = duration(func(s *settings) *string { return &s.Heartbeat }, false)
	add(settingField{Key: "heartbeat", Label: "Heartbeat interval", Kind: settingDuration, Live: true, Get: get, Set: set,
		Help: "How long after the last user message the private heartbeat runs. 0 disables heartbeats."})
	get, set = duration(func(s *settings) *string { return &s.RequestTimeout }, true)
	add(settingField{Key: "request_timeout", Label: "Request timeout", Kind: settingDuration, Live: true, Get: get, Set: set,
		Help: "Maximum time for one turn including its tool rounds."})
	get, set = boolean(func(s *settings) *bool { return &s.Debug })
	add(settingField{Key: "debug", Label: "Debug transcript", Kind: settingBool, Live: true, Get: get, Set: set,
		Help: "Show harness, system, and reasoning messages in the transcript."})
	get, set = boolean(func(s *settings) *bool { return &s.CompactTranscript })
	add(settingField{Key: "compact_transcript", Label: "Compact transcript", Kind: settingBool, Live: true, Get: get, Set: set,
		Help: "Dense one-line turns with initial labels instead of bordered blocks."})
	get, set = boolean(func(s *settings) *bool { return &s.LeanTools })
	add(settingField{Key: "lean_tools", Label: "Lean tools", Kind: settingBool, Live: true, Get: get, Set: set,
		Help: "Hide scheduling, memory-admin, images, and Whoop tools until the model requests them with request_toolcall."})
	get, set = boolean(func(s *settings) *bool { return &s.AllowPlaintextPreview })
	add(settingField{Key: "allow_plaintext_terminal_preview", Label: "Terminal camera preview", Kind: settingBool, Live: true, Get: get, Set: set,
		Help: "Allow ImageSnap terminal previews, which briefly write plaintext frames to disk."})
	add(settingField{Key: "compaction_history", Label: "Compaction history", Kind: settingInt, Live: true,
		Get: func(s *settings) string { return strconv.Itoa(s.CompactionHistory) },
		Set: func(s *settings, value string) error {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || parsed < 1 {
				return fmt.Errorf("must be at least 1")
			}
			s.CompactionHistory = parsed
			return nil
		},
		Help: "How many session summaries the exit compaction keeps. Older ones are offered to the model for archiving to memory, then deleted."})
	get, set = str(func(s *settings) *string { return &s.CompactionInstructions })
	add(settingField{Key: "compaction_instructions", Label: "Compaction instructions", Kind: settingMultiline, Live: true, Get: get, Set: set,
		Help: "Guidance appended to the exit compaction request, for example what to emphasise or ignore in summaries. Multi-line; Ctrl+S applies the edit."})
	get, set = str(func(s *settings) *string { return &s.CompactionInstructionsFile })
	add(settingField{Key: "compaction_instructions_file", Label: "Compaction instructions file", Kind: settingText, Live: true, Get: get, Set: set,
		Help: "Optional file whose text is appended after the inline instructions. Use it for longer guidance kept in a repository or notes."})
	get, set = boolean(func(s *settings) *bool { return &s.CompactionThinking })
	add(settingField{Key: "compaction_thinking", Label: "Compaction thinking", Kind: settingBool, Live: true, Get: get, Set: set,
		Help: "Keep model reasoning on during the exit compaction. Off is several times faster on llama.cpp; the chat's own thinking setting is unaffected."})
	get, set = str(func(s *settings) *string { return &s.OllamaURL })
	add(settingField{Key: "ollama_url", Label: "Ollama URL", Kind: settingText, Get: get, Set: set,
		Help: "OpenAI-compatible base URL. Used the next time models are discovered (r in the picker)."})
	get, set = str(func(s *settings) *string { return &s.LlamaURL })
	add(settingField{Key: "llama_url", Label: "llama.cpp URL", Kind: settingText, Get: get, Set: set,
		Help: "OpenAI-compatible base URL. Used the next time models are discovered (r in the picker)."})
	get, set = str(func(s *settings) *string { return &s.WhoopCallbackListen })
	add(settingField{Key: "whoop_callback_listen", Label: "Whoop callback listen", Kind: settingText, Get: get, Set: set,
		Help: "Local address that receives the Whoop OAuth callback forwarded by ngrok."})
	get, set = str(func(s *settings) *string { return &s.WhoopRedirectURL })
	add(settingField{Key: "whoop_redirect_url", Label: "Whoop redirect URL", Kind: settingText, Get: get, Set: set,
		Help: "Public HTTPS callback URL registered in the Whoop developer dashboard."})
	get, set = duration(func(s *settings) *string { return &s.WhoopAuthTimeout }, true)
	add(settingField{Key: "whoop_auth_timeout", Label: "Whoop auth timeout", Kind: settingDuration, Get: get, Set: set,
		Help: "How long /whoop waits for the OAuth callback."})
	return fields
}

func (m *appModel) openSettings() {
	m.settingsDraft = m.cfg.Settings
	m.settingsFields = settingsFields(m.choices)
	m.settingsCursor = 0
	m.settingsEditing = false
	m.settingsError = ""
	m.settingsReturn = m.phase
	m.phase = phaseSettings
	m.composer.Blur()
	m.status = "Settings · Enter edit · s save · esc cancel"
}

func (m *appModel) closeSettings() {
	m.phase = m.settingsReturn
	m.settingsEditing = false
	m.settingsError = ""
}

func (m *appModel) startSettingsEdit() tea.Cmd {
	field := m.settingsFields[m.settingsCursor]
	if field.Kind == settingMultiline {
		area := textarea.New()
		area.ShowLineNumbers = false
		area.Placeholder = "Guidance for the compaction agent…"
		area.CharLimit = 4_000
		area.SetWidth(max(24, min(m.width-30, 80)))
		area.SetHeight(5)
		area.SetValue(field.Get(&m.settingsDraft))
		area.MoveToEnd()
		m.settingsArea = area
		m.settingsEditing = true
		m.settingsError = ""
		return m.settingsArea.Focus()
	}
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 512
	input.SetWidth(max(20, min(m.width-30, 72)))
	input.SetValue(field.Get(&m.settingsDraft))
	input.SetCursor(len(input.Value()))
	m.settingsInput = input
	m.settingsEditing = true
	m.settingsError = ""
	return m.settingsInput.Focus()
}

func (m *appModel) commitSettingsEdit() {
	field := m.settingsFields[m.settingsCursor]
	value := m.settingsInput.Value()
	if field.Kind == settingMultiline {
		value = m.settingsArea.Value()
	}
	if err := field.Set(&m.settingsDraft, value); err != nil {
		m.settingsError = field.Label + ": " + err.Error()
		return
	}
	m.settingsEditing = false
	m.settingsError = ""
}

// cycleSetting advances a bool or enum row by step (+1 or -1).
func (m *appModel) cycleSetting(step int) {
	field := m.settingsFields[m.settingsCursor]
	switch field.Kind {
	case settingBool:
		if field.Get(&m.settingsDraft) == "true" {
			_ = field.Set(&m.settingsDraft, "false")
		} else {
			_ = field.Set(&m.settingsDraft, "true")
		}
	case settingEnum:
		if len(field.Options) == 0 {
			return
		}
		current := field.Get(&m.settingsDraft)
		index := 0
		for i, option := range field.Options {
			if option.Value == current {
				index = i
				break
			}
		}
		index = (index + step + len(field.Options)) % len(field.Options)
		_ = field.Set(&m.settingsDraft, field.Options[index].Value)
	}
	m.settingsError = ""
}

func (m *appModel) resetSettingToDefault() {
	field := m.settingsFields[m.settingsCursor]
	defaults := defaultSettings()
	_ = field.Set(&m.settingsDraft, field.Get(&defaults))
	m.settingsError = ""
}

func (m *appModel) saveSettings() tea.Cmd {
	if err := m.settingsDraft.validate(); err != nil {
		m.settingsError = err.Error()
		return nil
	}
	if err := saveSettings(m.cfg.SettingsPath, m.settingsDraft); err != nil {
		m.settingsError = "could not write " + m.cfg.SettingsPath + ": " + err.Error()
		return nil
	}
	cmd := m.applySettings(m.settingsDraft)
	m.closeSettings()
	m.status = "Settings saved to " + m.cfg.SettingsPath
	return cmd
}

// applySettings updates the running session with values that can change
// live, and records the rest for the next connection or discovery.
func (m *appModel) applySettings(value settings) tea.Cmd {
	previous := m.cfg
	m.cfg = value.toConfig(m.cfg)
	m.showDebug = value.Debug
	m.compactTranscript = value.CompactTranscript
	m.cameraAvailable = m.cfg.AllowPlaintextPreview && cameraBackendAvailable()
	m.memory.categoriesFile = m.cfg.MemoryCategoriesFile
	if m.client != nil {
		m.client.context = m.cfg.Context
		m.client.http.Timeout = m.cfg.RequestTimeout
		m.client.exposeAllTools = !m.cfg.LeanTools
	}
	var cmd tea.Cmd
	if m.cfg.Heartbeat != previous.Heartbeat && m.settingsReturn == phaseChatting {
		m.disarmHeartbeat()
		if m.cfg.Heartbeat > 0 {
			cmd = m.armDefaultHeartbeat(time.Now())
		}
	}
	m.resize()
	return cmd
}

func (m appModel) updateSettings(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	multiline := m.settingsEditing && m.settingsFields[m.settingsCursor].Kind == settingMultiline
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		if multiline {
			var cmd tea.Cmd
			m.settingsArea, cmd = m.settingsArea.Update(msg)
			commands = append(commands, cmd)
		} else if m.settingsEditing {
			var cmd tea.Cmd
			m.settingsInput, cmd = m.settingsInput.Update(msg)
			commands = append(commands, cmd)
		}
		return m, tea.Batch(commands...)
	}
	if m.settingsEditing {
		switch key.String() {
		case "enter":
			if multiline {
				m.settingsArea.InsertString("\n")
			} else {
				m.commitSettingsEdit()
			}
		case "ctrl+s":
			m.commitSettingsEdit()
		case "esc":
			m.settingsEditing = false
			m.settingsError = ""
		case "ctrl+c":
			return m, tea.Quit
		default:
			var cmd tea.Cmd
			if multiline {
				m.settingsArea, cmd = m.settingsArea.Update(msg)
			} else {
				m.settingsInput, cmd = m.settingsInput.Update(msg)
			}
			commands = append(commands, cmd)
		}
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		m.closeSettings()
		m.status = "Settings unchanged"
		if m.phase == phaseChatting {
			commands = append(commands, m.composer.Focus())
		}
	case "up", "k":
		if m.settingsCursor > 0 {
			m.settingsCursor--
		}
	case "down", "j":
		if m.settingsCursor < len(m.settingsFields)-1 {
			m.settingsCursor++
		}
	case "left", "h":
		m.cycleSetting(-1)
	case "right", "l", " ":
		m.cycleSetting(1)
	case "enter":
		switch field := m.settingsFields[m.settingsCursor]; field.Kind {
		case settingBool, settingEnum:
			m.cycleSetting(1)
		case settingAction:
			if field.Action != nil {
				field.Action(&m)
			}
		default:
			commands = append(commands, m.startSettingsEdit())
		}
	case "d":
		m.resetSettingToDefault()
	case "s", "ctrl+s":
		if cmd := m.saveSettings(); cmd != nil {
			commands = append(commands, cmd)
		}
		if m.phase == phaseChatting {
			commands = append(commands, m.composer.Focus())
		}
	}
	return m, tea.Batch(commands...)
}

func (m appModel) renderSettings() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Settings")
	subtitle := lipgloss.NewStyle().Foreground(m.palette.muted).Render(m.cfg.SettingsPath)
	labelWidth := 0
	for _, field := range m.settingsFields {
		labelWidth = max(labelWidth, lipgloss.Width(field.Label))
	}
	var rows []string
	for index, field := range m.settingsFields {
		selected := index == m.settingsCursor
		cursor := "  "
		labelStyle := lipgloss.NewStyle().Foreground(m.palette.text)
		if selected {
			cursor = "› "
			labelStyle = labelStyle.Bold(true).Foreground(m.palette.secondary)
		}
		label := labelStyle.Render(fmt.Sprintf("%-*s", labelWidth, field.Label))
		var value string
		if selected && m.settingsEditing && field.Kind == settingMultiline {
			indent := strings.Repeat(" ", labelWidth+4)
			lines := strings.Split(m.settingsArea.View(), "\n")
			for i := 1; i < len(lines); i++ {
				lines[i] = indent + lines[i]
			}
			value = strings.Join(lines, "\n")
		} else if selected && m.settingsEditing {
			value = m.settingsInput.View()
		} else {
			value = m.renderSettingValue(field, selected)
		}
		rows = append(rows, cursor+label+"  "+value)
	}
	help := ""
	if len(m.settingsFields) > 0 {
		help = lipgloss.NewStyle().Foreground(m.palette.muted).Width(max(30, min(m.width-16, 84))).Render(m.settingsFields[m.settingsCursor].Help)
	}
	footer := "↑/↓ move  ·  Enter edit, toggle, or open  ·  ←/→ change  ·  d default  ·  s save  ·  esc cancel"
	if m.settingsEditing && m.settingsFields[m.settingsCursor].Kind == settingMultiline {
		footer = "Enter newline  ·  Ctrl+S apply  ·  esc cancel edit"
	} else if m.settingsEditing {
		footer = "Type the new value  ·  Enter apply  ·  esc cancel edit"
	}
	body := title + "\n" + subtitle + "\n\n" + strings.Join(rows, "\n") + "\n\n" + help
	if m.settingsError != "" {
		body += "\n" + lipgloss.NewStyle().Foreground(m.palette.danger).Render("✗ "+m.settingsError)
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

func (m appModel) renderSettingValue(field settingField, selected bool) string {
	value := field.Get(&m.settingsDraft)
	switch field.Kind {
	case settingAction:
		count := 0
		if categories, err := m.memory.Categories(); err == nil {
			count = len(categories)
		}
		label := fmt.Sprintf("%d configured", count)
		if count == 0 {
			label = "free-form"
		}
		hint := lipgloss.NewStyle().Foreground(m.palette.muted).Render("  Enter to edit ›")
		return lipgloss.NewStyle().Foreground(m.palette.text).Render(label) + hint
	case settingMultiline:
		if strings.TrimSpace(value) == "" {
			return lipgloss.NewStyle().Foreground(m.palette.muted).Render("(none)")
		}
		lines := strings.Split(strings.TrimSpace(value), "\n")
		preview := lines[0]
		if len(lines) > 1 {
			preview += fmt.Sprintf("  … (+%d line%s)", len(lines)-1, pluralSuffix(len(lines)-1, "", "s"))
		}
		return lipgloss.NewStyle().Foreground(m.palette.text).Render(preview)
	case settingBool:
		on := lipgloss.NewStyle().Foreground(m.palette.muted).Render("○ on")
		off := lipgloss.NewStyle().Foreground(m.palette.muted).Render("○ off")
		if value == "true" {
			on = lipgloss.NewStyle().Bold(true).Foreground(m.palette.success).Render("● on")
		} else {
			off = lipgloss.NewStyle().Bold(true).Foreground(m.palette.accent).Render("● off")
		}
		return on + "   " + off
	case settingEnum:
		label := value
		for _, option := range field.Options {
			if option.Value == value {
				label = option.Label
			}
		}
		if label == "" {
			label = "(none)"
		}
		arrows := lipgloss.NewStyle().Foreground(m.palette.muted)
		chosen := lipgloss.NewStyle().Foreground(m.palette.text)
		if selected {
			chosen = chosen.Bold(true).Foreground(m.palette.secondary)
		}
		return arrows.Render("‹ ") + chosen.Render(label) + arrows.Render(" ›") +
			arrows.Render(fmt.Sprintf("  (%d choices)", len(field.Options)))
	default:
		if strings.TrimSpace(value) == "" {
			return lipgloss.NewStyle().Foreground(m.palette.muted).Render("(not set)")
		}
		return lipgloss.NewStyle().Foreground(m.palette.text).Render(value)
	}
}
