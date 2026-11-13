package main

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

type phase int

const (
	phaseDiscovering phase = iota
	phasePicking
	phaseChatting
)

type palette struct {
	primary   color.Color
	secondary color.Color
	accent    color.Color
	muted     color.Color
	surface   color.Color
	text      color.Color
	danger    color.Color
	success   color.Color
}

type appModel struct {
	cfg             appConfig
	phase           phase
	width           int
	height          int
	choices         []modelChoice
	cursor          int
	selected        modelChoice
	viewport        viewport.Model
	composer        textarea.Model
	spinner         spinner.Model
	transcript      []transcriptMessage
	messages        []apiMessage
	pendingImages   []string
	client          *apiClient
	busy            bool
	status          string
	err             error
	promptTokens    int
	palette         palette
	showDebug       bool
	streamSession   uint64
	streamCancel    context.CancelFunc
	streamEvents    <-chan completionStreamEvent
	streamAnswer    string
	streamReasoning string
}

type discoveredMsg struct {
	choices []modelChoice
}

type completionStreamMsg struct {
	session uint64
	events  <-chan completionStreamEvent
	event   completionStreamEvent
	ok      bool
}

type responseMsg struct {
	result completionResult
	err    error
}

func newAppModel(cfg appConfig) appModel {
	composer := textarea.New()
	composer.Placeholder = "Ask anything…  Enter sends · Shift+Enter adds a line"
	composer.ShowLineNumbers = false
	composer.SetHeight(4)
	composer.CharLimit = 32_000

	spin := spinner.New()
	spin.Spinner = spinner.Dot

	return appModel{
		cfg:           cfg,
		phase:         phaseDiscovering,
		viewport:      viewport.New(),
		composer:      composer,
		spinner:       spin,
		pendingImages: append([]string(nil), cfg.StartupImages...),
		status:        "Looking for local models…",
		showDebug:     cfg.Debug,
		palette: palette{
			primary:   lipgloss.Color("#A78BFA"),
			secondary: lipgloss.Color("#67E8F9"),
			accent:    lipgloss.Color("#F9A8D4"),
			muted:     lipgloss.Color("#7C8396"),
			surface:   lipgloss.Color("#202333"),
			text:      lipgloss.Color("#E8EAF2"),
			danger:    lipgloss.Color("#FB7185"),
			success:   lipgloss.Color("#86EFAC"),
		},
	}
}

func (m appModel) Init() tea.Cmd {
	return tea.Batch(m.discoverCmd(), m.spinner.Tick)
}

func (m appModel) discoverCmd() tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		return discoveredMsg{choices: discoverModels(ctx, cfg)}
	}
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var commands []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
	case discoveredMsg:
		m.choices = msg.choices
		m.cursor = 0
		m.phase = phasePicking
		m.status = fmt.Sprintf("%d local model route(s) found", len(msg.choices))
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		commands = append(commands, cmd)
	case completionStreamMsg:
		if msg.session != m.streamSession || m.phase != phaseChatting {
			return m, tea.Batch(commands...)
		}
		if m.streamEvents == nil {
			m.streamEvents = msg.events
		}
		if !msg.ok {
			m.stopCompletionStream()
			return m.Update(responseMsg{err: errors.New("completion stream closed unexpectedly")})
		}
		switch msg.event.Kind {
		case streamConnected:
			m.status = "Delivered"
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamReasoningDelta:
			followStream := m.viewport.AtBottom()
			m.streamReasoning += msg.event.Delta
			m.status = "Thinking…"
			m.refreshTranscript()
			if followStream {
				m.viewport.GotoBottom()
			}
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamContentDelta:
			followStream := m.viewport.AtBottom()
			m.streamAnswer += msg.event.Delta
			m.status = "Typing…"
			m.refreshTranscript()
			if followStream {
				m.viewport.GotoBottom()
			}
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamFinished:
			m.stopCompletionStream()
			return m.Update(responseMsg{result: msg.event.Result, err: msg.event.Err})
		}
	case responseMsg:
		followTranscript := m.viewport.AtBottom()
		m.busy = false
		if msg.err != nil {
			// Drop the unanswered user turn so it can simply be sent again.
			if last := len(m.messages) - 1; last >= 0 && m.messages[last].Role == "user" {
				m.messages = m.messages[:last]
			}
			m.err = msg.err
			m.status = "Request failed · send again when ready"
			m.transcript = append(m.transcript, transcriptMessage{
				Role:       "error",
				Content:    msg.err.Error(),
				OccurredAt: time.Now(),
			})
		} else {
			m.err = nil
			m.promptTokens = msg.result.Usage.PromptTokens
			m.messages = append(m.messages, apiMessage{
				Role:             "assistant",
				Content:          msg.result.Reply,
				ReasoningContent: msg.result.Reasoning,
			})
			if strings.TrimSpace(msg.result.Reasoning) != "" {
				m.transcript = append(m.transcript, transcriptMessage{
					Role:       "reasoning",
					Content:    msg.result.Reasoning,
					OccurredAt: time.Now(),
				})
			}
			m.transcript = append(m.transcript, transcriptMessage{
				Role:       "assistant",
				Content:    msg.result.Reply,
				OccurredAt: time.Now(),
			})
			m.status = "Ready"
		}
		m.refreshTranscript()
		if followTranscript {
			m.viewport.GotoBottom()
		}
		commands = append(commands, m.composer.Focus())
	}

	switch m.phase {
	case phaseDiscovering:
		if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case phasePicking:
		return m.updatePicker(msg, commands)
	case phaseChatting:
		return m.updateChat(msg, commands)
	}

	return m, tea.Batch(commands...)
}

func (m appModel) updatePicker(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c", "q", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.choices)-1 {
			m.cursor++
		}
	case "r":
		m.phase = phaseDiscovering
		m.status = "Refreshing local models…"
		commands = append(commands, m.discoverCmd())
	case "enter":
		if len(m.choices) > 0 {
			cmd, err := m.selectModel(m.choices[m.cursor])
			if err != nil {
				m.err = err
				m.status = err.Error()
			} else {
				commands = append(commands, cmd)
			}
		}
	}
	return m, tea.Batch(commands...)
}

func (m *appModel) selectModel(choice modelChoice) (tea.Cmd, error) {
	m.stopCompletionStream()
	var initial []apiMessage
	if m.cfg.SystemFile != "" {
		content, err := os.ReadFile(m.cfg.SystemFile)
		if err != nil {
			return nil, fmt.Errorf("read system file: %w", err)
		}
		if text := strings.TrimSpace(string(content)); text != "" {
			initial = append(initial, apiMessage{Role: "system", Content: text})
		}
	}
	m.selected = choice
	m.messages = initial
	m.client = newAPIClient(choice, m.cfg.Context, m.cfg.RequestTimeout)
	m.phase = phaseChatting
	m.busy = false
	m.err = nil
	m.promptTokens = 0
	m.transcript = []transcriptMessage{{
		Role:       "notice",
		Content:    fmt.Sprintf("Connected to %s through %s.", choice.Model, choice.Backend),
		OccurredAt: time.Now(),
	}}
	if len(initial) > 0 {
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "system",
			Content:    initial[0].Content.(string),
			OccurredAt: time.Now(),
			Meta:       filepath.Base(m.cfg.SystemFile),
		})
	}
	m.status = "Ready"
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return m.composer.Focus(), nil
}

func (m appModel) updateChat(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c":
			m.stopCompletionStream()
			return m, tea.Quit
		case "ctrl+g":
			if !m.busy {
				m.phase = phasePicking
				m.composer.Blur()
				m.status = "Choose a model · the current transcript will reset"
			}
			return m, tea.Batch(commands...)
		case "ctrl+d":
			m.toggleDebug()
			return m, tea.Batch(commands...)
		case "shift+enter", "ctrl+j":
			m.composer.InsertString("\n")
			return m, tea.Batch(commands...)
		case "pgup":
			m.viewport.PageUp()
			return m, tea.Batch(commands...)
		case "pgdown":
			m.viewport.PageDown()
			return m, tea.Batch(commands...)
		case "enter":
			if m.busy {
				if strings.TrimSpace(m.composer.Value()) != "" {
					m.status = "Draft saved · wait for the reply to finish, then press Enter"
				}
				return m, tea.Batch(commands...)
			}
			value := strings.TrimSpace(m.composer.Value())
			if value != "" {
				m.composer.Reset()
				if value == "/quit" || value == "/exit" {
					return m, tea.Quit
				}
				if cmd := m.submit(value); cmd != nil {
					commands = append(commands, cmd)
				}
			}
			return m, tea.Batch(commands...)
		}
	}

	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	commands = append(commands, cmd)
	// The viewport's default key map includes printable pager shortcuts, so
	// key presses go only to the composer. Non-key events (mouse wheel) still
	// reach the viewport.
	if _, isKeyPress := msg.(tea.KeyPressMsg); isKeyPress {
		return m, tea.Batch(commands...)
	}
	var viewportCmd tea.Cmd
	m.viewport, viewportCmd = m.viewport.Update(msg)
	commands = append(commands, viewportCmd)
	return m, tea.Batch(commands...)
}

func (m *appModel) submit(value string) tea.Cmd {
	if handled := m.handleCommand(value); handled {
		return nil
	}

	images := append([]string(nil), m.pendingImages...)
	var content any = value
	if len(images) > 0 {
		parts, err := imageParts(value, images)
		if err != nil {
			m.err = err
			m.status = err.Error()
			return nil
		}
		content = parts
	}
	m.pendingImages = nil
	m.messages = append(m.messages, apiMessage{Role: "user", Content: content})
	m.transcript = append(m.transcript, transcriptMessage{
		Role:       "user",
		Content:    value,
		Images:     baseNames(images),
		OccurredAt: time.Now(),
	})
	m.busy = true
	m.err = nil
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return m.startCompletionStream(append([]apiMessage(nil), m.messages...))
}

func (m *appModel) handleCommand(value string) bool {
	command, argument, _ := strings.Cut(value, " ")
	switch strings.ToLower(command) {
	case "/help":
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "notice",
			Content:    "Commands: /image PATH… · /clear-images · /system TEXT · /model · /clear · /debug [on|off] · /help · /quit\nKeys: Enter send · Shift+Enter newline (Ctrl+J fallback) · Ctrl+G models · Ctrl+D reasoning · PgUp/PgDn scroll · Ctrl+C quit",
			OccurredAt: time.Now(),
		})
	case "/debug":
		switch strings.ToLower(strings.TrimSpace(argument)) {
		case "", "toggle":
			m.showDebug = !m.showDebug
		case "on":
			m.showDebug = true
		case "off":
			m.showDebug = false
		default:
			m.status = "Usage: /debug [on|off]"
			return true
		}
		m.status = "Reasoning hidden"
		if m.showDebug {
			m.status = "Reasoning visible"
		}
	case "/model":
		m.phase = phasePicking
		m.composer.Blur()
		m.status = "Choose a model · the current transcript will reset"
	case "/clear":
		// Clears the visible transcript and the model's conversation history
		// alike, keeping only the system prompt.
		m.transcript = nil
		var kept []apiMessage
		for _, message := range m.messages {
			if message.Role == "system" {
				kept = append(kept, message)
			}
		}
		m.messages = kept
		m.promptTokens = 0
		m.status = "Conversation cleared"
	case "/clear-images":
		count := len(m.pendingImages)
		m.pendingImages = nil
		m.status = fmt.Sprintf("Cleared %d queued image(s)", count)
	case "/image":
		paths, err := splitArgs(argument)
		if err != nil {
			m.err = err
			m.status = err.Error()
			return true
		}
		if len(paths) == 0 {
			m.status = "Usage: /image PATH [PATH …]"
			return true
		}
		for _, path := range paths {
			resolved, err := resolveImage(path)
			if err != nil {
				m.err = err
				m.status = err.Error()
				return true
			}
			m.pendingImages = append(m.pendingImages, resolved)
		}
		m.status = fmt.Sprintf("%d image(s) queued for the next message", len(m.pendingImages))
	case "/system":
		if strings.TrimSpace(argument) == "" {
			m.status = "Usage: /system TEXT"
			return true
		}
		m.messages = append(m.messages, apiMessage{Role: "system", Content: strings.TrimSpace(argument)})
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "system",
			Content:    strings.TrimSpace(argument),
			OccurredAt: time.Now(),
		})
		m.status = "System context added"
	default:
		return false
	}
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return true
}

func (m *appModel) toggleDebug() {
	m.showDebug = !m.showDebug
	m.status = "Reasoning hidden"
	if m.showDebug {
		m.status = "Reasoning visible"
	}
	m.refreshTranscript()
	m.viewport.GotoBottom()
}

func (m *appModel) startCompletionStream(messages []apiMessage) tea.Cmd {
	m.stopCompletionStream()
	m.streamSession++
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	m.streamCancel = cancel
	m.status = "Sending…"
	session := m.streamSession
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return completionStreamMsg{
				session: session,
				event:   completionStreamEvent{Kind: streamFinished, Err: errors.New("no model selected")},
				ok:      true,
			}
		}
		events := client.completeStream(ctx, messages)
		event, ok := <-events
		return completionStreamMsg{session: session, events: events, event: event, ok: ok}
	}
}

func (m *appModel) stopCompletionStream() {
	if m.streamCancel != nil {
		m.streamCancel()
		m.streamCancel = nil
	}
	m.streamSession++
	m.streamEvents = nil
	m.streamAnswer = ""
	m.streamReasoning = ""
}

func waitCompletionStreamCmd(session uint64, events <-chan completionStreamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		return completionStreamMsg{session: session, events: events, event: event, ok: ok}
	}
}

func (m *appModel) resize() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	bodyWidth := max(24, m.width-4)
	m.composer.SetWidth(bodyWidth - 4)
	composerHeight := 6
	m.viewport.SetWidth(bodyWidth)
	m.viewport.SetHeight(max(4, m.height-composerHeight-6))
	m.refreshTranscript()
}

func (m *appModel) refreshTranscript() {
	if m.width <= 0 {
		return
	}
	messages := append([]transcriptMessage(nil), m.transcript...)
	if m.showDebug && strings.TrimSpace(m.streamReasoning) != "" {
		messages = append(messages, transcriptMessage{
			Role:       "reasoning",
			Content:    m.streamReasoning,
			OccurredAt: time.Now(),
			Meta:       "live",
		})
	}
	if m.streamAnswer != "" {
		messages = append(messages, transcriptMessage{
			Role:       "assistant_stream",
			Content:    m.streamAnswer,
			OccurredAt: time.Now(),
			Meta:       "streaming",
		})
	}
	m.viewport.SetContent(m.renderTranscriptMessages(messages, max(30, m.width-8)))
}

func (m appModel) View() tea.View {
	var content string
	switch m.phase {
	case phaseDiscovering:
		content = m.renderDiscovering()
	case phasePicking:
		content = m.renderPicker()
	case phaseChatting:
		content = m.renderChat()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func (m appModel) renderDiscovering() string {
	style := lipgloss.NewStyle().Foreground(m.palette.text).Padding(2, 4)
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("gemma4 · chat3")
	return style.Render(title + "\n\n" + m.spinner.View() + " " + m.status)
}

func (m appModel) renderPicker() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Choose your local model")
	subtitle := lipgloss.NewStyle().Foreground(m.palette.muted).Render(
		"Routes are discovered live from Ollama and llama.cpp.",
	)
	var rows []string
	for index, choice := range m.choices {
		cursor := "  "
		style := lipgloss.NewStyle().Foreground(m.palette.text).PaddingLeft(1)
		if index == m.cursor {
			cursor = "› "
			style = style.
				Bold(true).
				Foreground(m.palette.secondary).
				Background(m.palette.surface)
		}
		state := lipgloss.NewStyle().Foreground(m.palette.danger).Render("offline")
		if choice.Online {
			state = lipgloss.NewStyle().Foreground(m.palette.success).Render("online")
		}
		row := fmt.Sprintf("%s%s\n    %s · %s · %s", cursor, choice.Model, choice.Backend, choice.Detail, state)
		rows = append(rows, style.Width(max(30, m.width-10)).Render(row))
	}
	help := lipgloss.NewStyle().Foreground(m.palette.muted).Render("↑/↓ move  ·  enter connect  ·  r refresh  ·  q quit")
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.primary).
		Padding(1, 2).
		Width(max(40, min(m.width-8, 92))).
		Render(title + "\n" + subtitle + "\n\n" + strings.Join(rows, "\n") + "\n\n" + help)
	return lipgloss.NewStyle().Padding(2, 4).Render(panel)
}

func (m appModel) renderChat() string {
	composerBorder := m.palette.muted
	if !m.busy {
		composerBorder = m.palette.secondary
	}
	composer := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(composerBorder).
		Padding(0, 1).
		Width(max(20, m.width-4)).
		Render(m.composer.View())
	footerText := "Enter send  ·  Shift+Enter newline  ·  Ctrl+G models  ·  Ctrl+D reasoning  ·  PgUp/PgDn scroll  ·  Ctrl+C quit"
	if m.busy {
		footerText = "Model replying · keep drafting · wait for Ready, then Enter to send · PgUp/PgDn scroll · Ctrl+C quit"
	}
	if len(m.pendingImages) > 0 {
		footerText = fmt.Sprintf("%d image(s) queued · Enter sends all · /clear-images to drop them", len(m.pendingImages))
	}
	footer := lipgloss.NewStyle().Foreground(m.palette.muted).Render(footerText)
	return lipgloss.JoinVertical(lipgloss.Left, m.renderHeader(), m.viewport.View(), m.renderStatus(), composer, footer)
}

func (m appModel) renderHeader() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("gemma4 · chat3")
	model := lipgloss.NewStyle().Bold(true).Foreground(m.palette.text).Render(m.selected.Model)
	backend := lipgloss.NewStyle().Foreground(m.palette.secondary).Render(string(m.selected.Backend))
	contextPct := 0
	if m.cfg.Context > 0 {
		contextPct = int(math.Min(100, float64(m.promptTokens)/float64(m.cfg.Context)*100))
	}
	right := fmt.Sprintf("%d%% ctx", contextPct)
	if len(m.pendingImages) > 0 {
		right += fmt.Sprintf("  ·  %d image(s)", len(m.pendingImages))
	}
	if m.showDebug {
		right += "  ·  REASONING"
	}
	left := title + "  " + model + "  " + backend
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right)-2)
	return lipgloss.NewStyle().
		Background(m.palette.surface).
		Foreground(m.palette.text).
		Width(max(20, m.width)).
		Padding(0, 1).
		Render(left + strings.Repeat(" ", gap) + right)
}

func (m appModel) renderStatus() string {
	text := m.status
	color := m.palette.muted
	if m.busy {
		text = m.spinner.View() + " " + text
		color = m.palette.accent
	}
	if m.err != nil {
		color = m.palette.danger
	}
	return lipgloss.NewStyle().Foreground(color).Padding(0, 1).Render(text)
}

func (m appModel) renderTranscriptMessages(messages []transcriptMessage, width int) string {
	var blocks []string
	for _, message := range messages {
		if (message.Role == "system" || message.Role == "reasoning") && !m.showDebug {
			continue
		}
		label := strings.ToUpper(message.Role)
		color := m.palette.muted
		switch message.Role {
		case "user":
			color = m.palette.secondary
		case "assistant", "assistant_stream":
			color = m.palette.primary
			label = "ASSISTANT"
		case "reasoning":
			color = m.palette.accent
			label = "ASSISTANT · THINKING"
		case "notice":
			color = m.palette.success
		case "error":
			color = m.palette.danger
		}
		meta := message.OccurredAt.Format("15:04")
		if message.Meta != "" {
			meta += " · " + message.Meta
		}
		heading := lipgloss.NewStyle().Bold(true).Foreground(color).Render(label)
		timeText := lipgloss.NewStyle().Foreground(m.palette.muted).Render(meta)
		body := message.Content
		if message.Role == "assistant" {
			body = renderMarkdown(body, width-4)
		} else {
			body = lipgloss.NewStyle().Foreground(m.palette.text).Width(width - 4).Render(body)
		}
		if len(message.Images) > 0 {
			body += "\n" + lipgloss.NewStyle().Foreground(m.palette.accent).Render("◈ "+strings.Join(message.Images, "  ◈ "))
		}
		block := lipgloss.NewStyle().
			BorderLeft(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(color).
			PaddingLeft(1).
			Width(width).
			Render(heading + "  " + timeText + "\n" + body)
		blocks = append(blocks, block)
	}
	return strings.Join(blocks, "\n\n")
}

func renderMarkdown(content string, width int) string {
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(max(20, width)),
	)
	if err != nil {
		return content
	}
	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimSpace(rendered)
}

func resolveImage(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("cannot read image %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("image is not a file: %s", absolute)
	}
	switch strings.ToLower(filepath.Ext(absolute)) {
	case ".bmp", ".gif", ".jpeg", ".jpg", ".png", ".tga":
		return absolute, nil
	default:
		return "", fmt.Errorf("unsupported image type: %s", filepath.Ext(absolute))
	}
}

func splitArgs(input string) ([]string, error) {
	var (
		args    []string
		current strings.Builder
		quote   rune
		escaped bool
	)
	flush := func() {
		if current.Len() > 0 {
			args = append(args, current.String())
			current.Reset()
		}
	}
	for _, char := range input {
		if escaped {
			current.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case ' ', '\t', '\n':
			flush()
		default:
			current.WriteRune(char)
		}
	}
	if escaped {
		current.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in image path")
	}
	flush()
	return args, nil
}

func baseNames(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		result = append(result, filepath.Base(path))
	}
	return result
}
