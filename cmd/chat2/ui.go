package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/filepicker"
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
	phaseStarting
	phaseChatting
	phaseImageRequest
	phaseCameraWindow
	phaseCameraPreview
	phaseFilePicking
	phasePhotoReview
)

type deliveryStage int

const defaultHeartbeatInterval = 15 * time.Minute

const (
	deliveryIdle deliveryStage = iota
	deliverySending
	deliveryDelivered
	deliveryTyping
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
	cfg                 appConfig
	phase               phase
	width               int
	height              int
	choices             []modelChoice
	cursor              int
	selected            modelChoice
	viewport            viewport.Model
	composer            textarea.Model
	filePicker          filepicker.Model
	spinner             spinner.Model
	transcript          []transcriptMessage
	restoredTranscript  []transcriptMessage
	messages            []apiMessage
	pendingImages       []string
	captureVault        *captureVault
	client              *apiClient
	memory              *memoryStore
	schedule            *scheduleStore
	memoryCount         int
	busy                bool
	status              string
	err                 error
	promptTokens        int
	lastSent            time.Time
	palette             palette
	imageRequest        *imageRequest
	requestPrompt       string
	requestImages       []string
	requestHeartbeat    bool
	requestVisibleReply bool
	capturePath         string
	captureOwned        bool
	captureManual       bool
	cameraAvailable     bool
	previewFrame        string
	previewANSI         string
	previewSession      uint64
	previewLoading      bool
	previewCancel       context.CancelFunc
	windowSession       uint64
	windowCancel        context.CancelFunc
	startupSession      uint64
	startupCancel       context.CancelFunc
	userLabel           string
	assistantLabel      string
	showDebug           bool
	streamSession       uint64
	streamCancel        context.CancelFunc
	streamEvents        <-chan completionStreamEvent
	streamAnswer        string
	streamReasoning     string
	streamPrompt        string
	streamImages        []string
	streamHeartbeat     bool
	streamVisibleReply  bool
	seenCompletions     map[int]struct{}
	deliveryStage       deliveryStage
	deliverySequence    uint64
	heartbeatGeneration uint64
	heartbeatArmed      bool
	heartbeatDue        time.Time
	heartbeatExplicit   bool
}

type discoveredMsg struct {
	choices []modelChoice
}

type responseMsg struct {
	result    completionResult
	prompt    string
	images    []string
	heartbeat bool
	err       error
}

type startupMsg struct {
	session uint64
	result  startupResult
	err     error
}

type whoopAuthMsg struct {
	err error
}

type completionStreamMsg struct {
	session uint64
	events  <-chan completionStreamEvent
	event   completionStreamEvent
	ok      bool
}

type heartbeatMsg struct {
	generation uint64
	at         time.Time
}

type deliveryTypingMsg struct {
	session  uint64
	sequence uint64
}

type cameraPreviewMsg struct {
	session uint64
	path    string
	ansi    string
	err     error
}

type cameraPreviewTickMsg struct{ session uint64 }

type cameraWindowResultMsg struct {
	session   uint64
	path      string
	cancelled bool
	err       error
}

func newAppModel(cfg appConfig, memory *memoryStore) appModel {
	composer := textarea.New()
	composer.Placeholder = "Ask anything…  Enter sends · Ctrl+J adds a line"
	composer.ShowLineNumbers = false
	composer.SetHeight(4)
	composer.CharLimit = 32_000

	spin := spinner.New()
	spin.Spinner = spinner.Dot

	picker := filepicker.New()
	if home, err := os.UserHomeDir(); err == nil {
		picker.CurrentDirectory = home
	}
	picker.AllowedTypes = []string{".bmp", ".gif", ".jpeg", ".jpg", ".png", ".tga"}
	picker.ShowHidden = false
	picker.SetHeight(12)

	colors := palette{
		primary:   lipgloss.Color("#A78BFA"),
		secondary: lipgloss.Color("#67E8F9"),
		accent:    lipgloss.Color("#F9A8D4"),
		muted:     lipgloss.Color("#7C8396"),
		surface:   lipgloss.Color("#202333"),
		text:      lipgloss.Color("#E8EAF2"),
		danger:    lipgloss.Color("#FB7185"),
		success:   lipgloss.Color("#86EFAC"),
	}

	return appModel{
		cfg:             cfg,
		phase:           phaseDiscovering,
		viewport:        viewport.New(),
		composer:        composer,
		filePicker:      picker,
		spinner:         spin,
		memory:          memory,
		schedule:        newScheduleStore(memory.dir),
		pendingImages:   append([]string(nil), cfg.StartupImages...),
		captureVault:    newCaptureVault(),
		lastSent:        memory.LastUserActivityTime(),
		status:          "Looking for local models…",
		palette:         colors,
		cameraAvailable: cfg.AllowPlaintextPreview && cameraBackendAvailable(),
		userLabel:       "USER",
		assistantLabel:  "ASSISTANT",
		showDebug:       cfg.Debug,
		seenCompletions: make(map[int]struct{}),
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
		m.phase = phasePicking
		m.status = fmt.Sprintf("%d local model route(s) found", len(msg.choices))
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		commands = append(commands, cmd)
	case startupMsg:
		if msg.session != m.startupSession || m.phase != phaseStarting {
			return m, tea.Batch(commands...)
		}
		m.startupCancel = nil
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Startup protocol failed · r retry · esc models"
		} else {
			m.err = nil
			startupDebug := startupDebugMessages(m.messages, msg.result.Messages)
			m.messages = msg.result.Messages
			m.userLabel = msg.result.Labels.User
			m.assistantLabel = msg.result.Labels.Assistant
			m.promptTokens = msg.result.Usage.PromptTokens
			m.phase = phaseChatting
			m.transcript = startupDebugTranscript(startupDebug, m.assistantLabel)
			m.transcript = append(m.transcript, m.restoredTranscript...)
			m.transcript = append(m.transcript, transcriptMessage{
				Role: "system",
				Content: fmt.Sprintf(
					"Connected to %s through %s.\nSession labels: %s ↔ %s.",
					m.selected.Model,
					m.selected.Backend,
					m.userLabel,
					m.assistantLabel,
				),
				OccurredAt: time.Now(),
				Meta:       fmt.Sprintf("%d memories loaded", m.memoryCount),
			})
			m.status = "Ready"
			m.refreshTranscript()
			m.viewport.GotoBottom()
			commands = append(commands, m.composer.Focus())
			if m.cfg.Heartbeat > 0 {
				commands = append(commands, m.armHeartbeatAfterLastUser(time.Now()))
			}
		}
	case whoopAuthMsg:
		m.busy = false
		m.err = msg.err
		content := "Whoop is connected. Recovery and sleep access is now available to the assistant."
		if msg.err != nil {
			content = "Whoop setup failed: " + msg.err.Error()
			m.status = "Whoop setup failed"
		} else {
			m.status = "Whoop connected"
		}
		m.transcript = append(m.transcript, transcriptMessage{Role: "notice", Content: content, OccurredAt: time.Now()})
		m.refreshTranscript()
		m.viewport.GotoBottom()
		commands = append(commands, m.composer.Focus())
	case completionStreamMsg:
		if msg.session != m.streamSession || m.phase != phaseChatting {
			return m, tea.Batch(commands...)
		}
		if m.streamEvents == nil {
			m.streamEvents = msg.events
		}
		if !msg.ok {
			prompt, images, heartbeat := m.streamPrompt, append([]string(nil), m.streamImages...), m.streamHeartbeat
			m.stopCompletionStream()
			m.busy = false
			return m.Update(responseMsg{
				prompt: prompt, images: images, heartbeat: heartbeat,
				err: errors.New("completion stream closed unexpectedly"),
			})
		}
		switch msg.event.Kind {
		case streamConnected:
			m.deliverySequence++
			m.deliveryStage = deliveryDelivered
			m.status = "Delivered"
			commands = append(commands,
				waitCompletionStreamCmd(msg.session, m.streamEvents),
				deliveryTypingCmd(msg.session, m.deliverySequence),
			)
		case streamReasoningDelta:
			m.streamReasoning += msg.event.Delta
			if m.deliveryStage != deliveryDelivered {
				m.deliveryStage = deliveryTyping
				m.status = "Typing…"
			}
			m.refreshTranscript()
			m.viewport.GotoBottom()
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamContentDelta:
			m.streamAnswer += msg.event.Delta
			if m.deliveryStage != deliveryDelivered {
				m.deliveryStage = deliveryTyping
				m.status = "Typing…"
			}
			m.refreshTranscript()
			m.viewport.GotoBottom()
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamToolRound:
			m.deliverySequence++
			m.deliveryStage = deliveryIdle
			if strings.TrimSpace(msg.event.Reasoning) != "" {
				m.transcript = append(m.transcript, transcriptMessage{
					Role:       "reasoning",
					Content:    msg.event.Reasoning,
					OccurredAt: time.Now(),
					Meta:       "tool round",
				})
			}
			if strings.TrimSpace(msg.event.Content) != "" {
				m.transcript = append(m.transcript, transcriptMessage{
					Role:       "assistant",
					Content:    msg.event.Content,
					OccurredAt: time.Now(),
					Meta:       "before tool call",
				})
			}
			m.transcript = append(m.transcript, transcriptMessage{
				Role:       "system",
				Content:    "Tool call: " + strings.Join(msg.event.ToolNames, ", "),
				OccurredAt: time.Now(),
			})
			m.streamAnswer = ""
			m.streamReasoning = ""
			m.status = "Using " + strings.Join(msg.event.ToolNames, ", ") + "…"
			m.refreshTranscript()
			m.viewport.GotoBottom()
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamFinished:
			prompt, images, heartbeat := m.streamPrompt, append([]string(nil), m.streamImages...), m.streamHeartbeat
			msg.event.Result.HadVisibleReply = msg.event.Result.HadVisibleReply || m.streamVisibleReply
			if strings.TrimSpace(msg.event.Result.Reasoning) != "" {
				meta := ""
				if heartbeat {
					meta = "heartbeat"
				}
				m.transcript = append(m.transcript, transcriptMessage{
					Role:       "reasoning",
					Content:    msg.event.Result.Reasoning,
					OccurredAt: time.Now(),
					Meta:       meta,
				})
			}
			m.stopCompletionStream()
			if heartbeat && msg.event.Err == nil {
				trimmed := strings.TrimSpace(msg.event.Result.Reply)
				if trimmed == "" {
					msg.event.Result.Reply = ""
				}
			}
			return m.Update(responseMsg{
				result: msg.event.Result,
				prompt: prompt, images: images, heartbeat: heartbeat,
				err: msg.event.Err,
			})
		}
	case deliveryTypingMsg:
		if msg.session == m.streamSession && msg.sequence == m.deliverySequence && m.phase == phaseChatting && m.busy && m.deliveryStage == deliveryDelivered {
			m.deliveryStage = deliveryTyping
			m.status = "Typing…"
		}
	case responseMsg:
		m.busy = false
		if msg.err == nil && msg.result.ImageRequest != nil {
			if !msg.heartbeat && msg.result.NextHeartbeat != nil {
				commands = append(commands, m.armExplicitHeartbeatAt(*msg.result.NextHeartbeat))
			}
			m.messages = msg.result.Messages
			m.imageRequest = msg.result.ImageRequest
			m.requestPrompt = msg.prompt
			m.requestImages = append([]string(nil), msg.images...)
			m.requestHeartbeat = msg.heartbeat
			m.requestVisibleReply = msg.result.HadVisibleReply
			m.captureManual = false
			m.capturePath = ""
			m.phase = phaseImageRequest
			m.composer.Blur()
			m.status = "The model is requesting an image · awaiting your consent"
			return m, tea.Batch(commands...)
		}
		if msg.heartbeat {
			if len(msg.result.Messages) > 0 {
				m.messages = discardEmbeddedImages(msg.result.Messages)
			} else {
				m.messages = discardEmbeddedImages(m.messages)
			}
			if msg.err != nil {
				m.status = "Ready · heartbeat failed"
				if m.showDebug {
					m.status += ": " + msg.err.Error()
				}
			} else if strings.TrimSpace(msg.result.Reply) == "" {
				m.status = "Ready"
			} else {
				m.messages = append(m.messages, apiMessage{Role: "assistant", Content: msg.result.Reply, ReasoningContent: msg.result.Reasoning})
				m.transcript = append(m.transcript, transcriptMessage{
					Role:       "assistant",
					Content:    msg.result.Reply,
					OccurredAt: time.Now(),
					Meta:       "heartbeat",
				})
				if err := m.memory.AppendRecentMessage("assistant", msg.result.Reply); err != nil {
					m.transcript = append(m.transcript, transcriptMessage{
						Role:       "error",
						Content:    "Could not persist recent response: " + err.Error(),
						OccurredAt: time.Now(),
					})
				}
				m.status = "Heartbeat surfaced a message"
				m.refreshTranscript()
				m.viewport.GotoBottom()
			}
			acted := msg.err == nil && (msg.result.HadVisibleReply || strings.TrimSpace(msg.result.Reply) != "")
			if !acted && m.cfg.Heartbeat > 0 {
				commands = append(commands, m.armDefaultHeartbeat(time.Now()))
			}
			commands = append(commands, m.composer.Focus())
			return m, tea.Batch(commands...)
		}
		if msg.err != nil {
			m.messages = discardEmbeddedImages(m.messages)
			m.err = msg.err
			m.status = "Request failed — press Enter to try again"
			m.transcript = append(m.transcript, transcriptMessage{
				Role:       "error",
				Content:    msg.err.Error(),
				OccurredAt: time.Now(),
			})
		} else {
			m.err = nil
			m.pendingImages = nil
			m.promptTokens = msg.result.Usage.PromptTokens
			m.messages = append(discardEmbeddedImages(msg.result.Messages), apiMessage{Role: "assistant", Content: msg.result.Reply, ReasoningContent: msg.result.Reasoning})
			m.transcript = append(m.transcript, transcriptMessage{
				Role:       "assistant",
				Content:    msg.result.Reply,
				OccurredAt: time.Now(),
				Meta:       strings.Join(msg.result.Notices, " · "),
			})
			m.status = "Ready"
			if err := m.memory.AppendRecentMessage("assistant", msg.result.Reply); err != nil {
				m.status = "Ready · could not persist recent response"
			}
		}
		m.refreshTranscript()
		m.viewport.GotoBottom()
		if msg.err == nil && msg.result.NextHeartbeat != nil {
			commands = append(commands, m.armExplicitHeartbeatAt(*msg.result.NextHeartbeat))
		}
		commands = append(commands, m.composer.Focus())
	case cameraPreviewMsg:
		if msg.session != m.previewSession || m.phase != phaseCameraPreview {
			if msg.path != "" {
				if err := removeCaptureFile(msg.path); err != nil {
					m.err = err
					m.status = err.Error()
				}
			}
			return m, tea.Batch(commands...)
		}
		m.previewLoading = false
		m.previewCancel = nil
		if msg.err != nil {
			m.err = msg.err
			m.status = "Camera preview failed · press r to retry or esc to cancel"
		} else {
			m.err = nil
			if m.previewFrame != "" {
				if err := removeCaptureFile(m.previewFrame); err != nil {
					m.err = err
					m.status = err.Error()
					_ = removeCaptureFile(msg.path)
					return m, tea.Batch(commands...)
				}
			}
			m.previewFrame = msg.path
			m.previewANSI = msg.ansi
			m.status = "Live preview · Space or Enter captures this frame"
			commands = append(commands, cameraPreviewTickCmd(msg.session))
		}
	case cameraPreviewTickMsg:
		if msg.session == m.previewSession && m.phase == phaseCameraPreview && !m.previewLoading {
			m.previewLoading = true
			commands = append(commands, m.cameraPreviewCmd(msg.session))
		}
	case cameraWindowResultMsg:
		if msg.session != m.windowSession || m.phase != phaseCameraWindow {
			if msg.path != "" {
				if err := removeCaptureFile(msg.path); err != nil {
					m.err = err
					m.status = err.Error()
				}
			}
			return m, tea.Batch(commands...)
		}
		m.windowCancel = nil
		if msg.err != nil {
			m.err = msg.err
			m.phase = phaseImageRequest
			m.status = "Camera window failed · use t for terminal preview"
		} else if msg.cancelled {
			m.phase = phaseImageRequest
			m.status = "Camera window cancelled · no photo was kept"
		} else {
			m.err = nil
			m.capturePath = msg.path
			m.captureOwned = true
			m.phase = phasePhotoReview
			m.status = "Photo received from camera window · review before sharing"
		}
	case heartbeatMsg:
		if msg.generation != m.heartbeatGeneration || !m.heartbeatArmed {
			break
		}
		m.heartbeatArmed = false
		m.heartbeatDue = time.Time{}
		m.heartbeatExplicit = false
		if m.phase == phaseChatting && !m.busy && m.cfg.Heartbeat > 0 {
			m.busy = true
			m.status = "Heartbeat…"
			now := msg.at
			commands = append(commands, m.sendCmd(m.responseEnvelope("", now), nil, true))
		} else if m.cfg.Heartbeat > 0 {
			commands = append(commands, m.armDefaultHeartbeat(msg.at))
		}
	}

	switch m.phase {
	case phaseDiscovering:
		if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case phasePicking:
		return m.updatePicker(msg, commands)
	case phaseStarting:
		return m.updateStarting(msg, commands)
	case phaseChatting:
		return m.updateChat(msg, commands)
	case phaseImageRequest:
		return m.updateImageRequest(msg, commands)
	case phaseCameraWindow:
		return m.updateCameraWindow(msg, commands)
	case phaseCameraPreview:
		return m.updateCameraPreview(msg, commands)
	case phaseFilePicking:
		return m.updateFilePicker(msg, commands)
	case phasePhotoReview:
		return m.updatePhotoReview(msg, commands)
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
	m.stopStartup()
	m.disarmHeartbeat()
	initial, recent, memoryCount, err := m.memory.InitialSystemMessages(m.cfg.SystemFile)
	if err != nil {
		return nil, err
	}
	m.selected = choice
	m.messages = initial
	m.restoredTranscript = recentMessagesTranscript(recent)
	m.memoryCount = memoryCount
	m.client = newAPIClient(choice, m.cfg.Context, m.memory, m.cfg.RequestTimeout)
	m.phase = phaseStarting
	m.transcript = nil
	m.userLabel = "USER"
	m.assistantLabel = "ASSISTANT"
	m.seenCompletions = make(map[int]struct{})
	m.status = "communicating with remote..."
	m.busy = true
	m.err = nil
	m.composer.Blur()
	return m.startupCmd(), nil
}

func (m appModel) updateStarting(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		m.stopStartup()
		return m, tea.Quit
	case "esc":
		m.stopStartup()
		m.busy = false
		m.phase = phasePicking
		m.status = "Choose a model"
	case "r":
		if !m.busy {
			m.err = nil
			m.busy = true
			m.status = "communicating with remote..."
			commands = append(commands, m.startupCmd())
		}
	}
	return m, tea.Batch(commands...)
}

func (m *appModel) startupCmd() tea.Cmd {
	m.stopStartup()
	m.startupSession++
	ctx, cancel := context.WithCancel(context.Background())
	m.startupCancel = cancel
	session := m.startupSession
	client := m.client
	messages := append([]apiMessage(nil), m.messages...)
	return func() tea.Msg {
		requestCtx, stop := context.WithTimeout(ctx, m.cfg.RequestTimeout)
		defer stop()
		result, err := client.startup(requestCtx, messages)
		return startupMsg{session: session, result: result, err: err}
	}
}

func (m *appModel) stopStartup() {
	if m.startupCancel != nil {
		m.startupCancel()
		m.startupCancel = nil
	}
	m.startupSession++
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
		case "ctrl+j":
			if !m.busy {
				m.composer.InsertString("\n")
			}
			return m, tea.Batch(commands...)
		case "pgup":
			m.viewport.PageUp()
			return m, tea.Batch(commands...)
		case "pgdown":
			m.viewport.PageDown()
			return m, tea.Batch(commands...)
		case "enter":
			if !m.busy {
				value := strings.TrimSpace(m.composer.Value())
				if value != "" {
					m.composer.Reset()
					if value == "/quit" || value == "/exit" {
						return m, tea.Quit
					}
					cmd := m.submit(value)
					if cmd != nil {
						commands = append(commands, cmd)
					}
				}
			}
			return m, tea.Batch(commands...)
		}
	}

	if !m.busy {
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
		commands = append(commands, cmd)
	}
	// The viewport's default key map includes printable pager shortcuts such as
	// j, k, u, d, f, b, h, l, and space. While the composer owns the keyboard,
	// forwarding those keys to both widgets makes ordinary typing scroll the
	// transcript. Page keys are handled explicitly above; non-key events still
	// reach the viewport so mouse-wheel scrolling continues to work.
	if _, isKeyPress := msg.(tea.KeyPressMsg); isKeyPress {
		return m, tea.Batch(commands...)
	}
	var viewportCmd tea.Cmd
	m.viewport, viewportCmd = m.viewport.Update(msg)
	commands = append(commands, viewportCmd)
	return m, tea.Batch(commands...)
}

func (m appModel) updateImageRequest(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	if m.busy {
		if key.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "c":
		m.err = nil
		commands = append(commands, m.startCameraWindow())
	case "t":
		if !m.cameraAvailable {
			if !m.cfg.AllowPlaintextPreview {
				m.err = errors.New("terminal preview is disabled because ImageSnap briefly writes plaintext frames; restart with --allow-plaintext-terminal-preview to opt in")
			} else {
				m.err = fmt.Errorf("terminal camera backend unavailable; install ImageSnap with: brew install imagesnap")
			}
			m.status = m.err.Error()
			return m, tea.Batch(commands...)
		}
		m.err = nil
		commands = append(commands, m.startCameraPreview())
	case "f":
		m.phase = phaseFilePicking
		m.err = nil
		m.status = "Choose an image · q returns without sharing"
		commands = append(commands, m.filePicker.Init())
	case "esc", "d":
		cmd := m.finishImageRequest("", true)
		if cmd != nil {
			commands = append(commands, cmd)
		}
	}
	return m, tea.Batch(commands...)
}

func (m appModel) updateCameraWindow(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		m.stopCameraWindow()
		return m, tea.Quit
	case "esc", "q":
		m.stopCameraWindow()
		m.phase = phaseImageRequest
		m.status = "Camera window cancelled · no photo was kept"
	}
	return m, tea.Batch(commands...)
}

func (m appModel) updateCameraPreview(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		m.stopCameraPreview(true)
		return m, tea.Quit
	case " ", "enter":
		if m.previewFrame == "" {
			m.status = "Waiting for the first camera frame…"
			return m, tea.Batch(commands...)
		}
		path, err := m.keepPreviewFrame()
		if err != nil {
			m.err = err
			m.status = err.Error()
			return m, tea.Batch(commands...)
		}
		m.capturePath = path
		m.captureOwned = true
		m.phase = phasePhotoReview
		m.status = "Photo captured · review before sharing"
	case "r":
		m.err = nil
		m.status = "Refreshing camera preview…"
		commands = append(commands, m.startCameraPreview())
	case "f":
		m.stopCameraPreview(true)
		m.phase = phaseFilePicking
		m.err = nil
		m.status = "Choose an image · q returns without sharing"
		commands = append(commands, m.filePicker.Init())
	case "esc", "q":
		m.stopCameraPreview(true)
		m.phase = phaseImageRequest
		m.status = "Camera preview cancelled · no photo was kept"
	}
	return m, tea.Batch(commands...)
}

func (m appModel) updateFilePicker(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			m.phase = phaseImageRequest
			m.status = "Image selection cancelled"
			return m, tea.Batch(commands...)
		}
	}
	if selected, path := m.filePicker.DidSelectFile(msg); selected {
		resolved, err := resolveImage(path)
		if err != nil {
			m.err = err
			m.status = err.Error()
			return m, tea.Batch(commands...)
		}
		m.capturePath = resolved
		m.captureOwned = false
		m.phase = phasePhotoReview
		m.status = "Image selected · review before sharing"
		return m, tea.Batch(commands...)
	}
	var cmd tea.Cmd
	m.filePicker, cmd = m.filePicker.Update(msg)
	commands = append(commands, cmd)
	return m, tea.Batch(commands...)
}

func (m appModel) updatePhotoReview(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	if m.busy {
		if key.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		_ = m.discardOwnedCapture()
		return m, tea.Quit
	case "s", "enter":
		cmd := m.finishImageRequest(m.capturePath, false)
		if cmd != nil {
			commands = append(commands, cmd)
		}
	case "r":
		if err := m.discardOwnedCapture(); err != nil {
			m.err = err
			m.status = err.Error()
			return m, tea.Batch(commands...)
		}
		m.status = "Opening camera window…"
		commands = append(commands, m.startCameraWindow())
	case "f":
		if err := m.discardOwnedCapture(); err != nil {
			m.err = err
			m.status = err.Error()
			return m, tea.Batch(commands...)
		}
		m.phase = phaseFilePicking
		m.status = "Choose a different image · q returns"
		commands = append(commands, m.filePicker.Init())
	case "esc", "d":
		cmd := m.finishImageRequest("", true)
		if cmd != nil {
			commands = append(commands, cmd)
		}
	}
	return m, tea.Batch(commands...)
}

func (m *appModel) beginManualCapture() {
	m.imageRequest = &imageRequest{
		Prompt: "Take or choose a photo to attach to your next message.",
		Reason: "You opened the photo capture tool manually.",
	}
	m.requestPrompt = ""
	m.requestImages = nil
	m.requestHeartbeat = false
	m.captureManual = true
	m.capturePath = ""
	m.captureOwned = false
	m.stopCameraPreview(true)
	m.stopCameraWindow()
	m.err = nil
	m.phase = phaseImageRequest
	m.composer.Blur()
	m.status = "Photo capture · nothing is shared until you approve it"
}

func (m *appModel) finishImageRequest(path string, declined bool) tea.Cmd {
	request := m.imageRequest
	if declined || path == "" {
		if err := m.discardOwnedCapture(); err != nil {
			m.err = err
			m.status = err.Error()
			return nil
		}
	}
	if m.captureManual {
		m.resetCaptureState()
		m.phase = phaseChatting
		if declined || path == "" {
			m.status = "Photo capture cancelled"
		} else {
			m.pendingImages = append(m.pendingImages, path)
			m.status = fmt.Sprintf("Photo queued for your next message: %s", filepath.Base(path))
			m.transcript = append(m.transcript, transcriptMessage{
				Role:       "system",
				Content:    "A camera photo was captured and queued. It has not been sent yet.",
				Images:     []string{filepath.Base(path)},
				OccurredAt: time.Now(),
			})
			m.refreshTranscript()
			m.viewport.GotoBottom()
		}
		return m.composer.Focus()
	}

	if request == nil || request.ToolCallID == "" {
		m.phase = phaseChatting
		m.status = "Image request state was lost"
		return m.composer.Focus()
	}

	if declined || path == "" {
		m.messages = append(m.messages, apiMessage{
			Role:       "tool",
			ToolCallID: request.ToolCallID,
			Content:    "The user declined to provide an image. Continue without one and do not ask again unless the user offers.",
		})
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "system",
			Content:    "Image request declined. The model will continue without a photo.",
			OccurredAt: time.Now(),
		})
	} else {
		parts, err := imagePartsWithVault("Here is the image you requested: "+request.Prompt, []string{path}, m.captureVault)
		if err != nil {
			m.err = err
			m.status = err.Error()
			return nil
		}
		if m.captureOwned {
			if err := m.removeOwnedCaptureFile(path); err != nil {
				m.err = err
				m.status = err.Error()
				return nil
			}
			m.captureOwned = false
		}
		m.messages = append(m.messages, apiMessage{
			Role:       "tool",
			ToolCallID: request.ToolCallID,
			Content:    "The user approved the request and supplied an image in the following user message.",
		})
		m.messages = append(m.messages, apiMessage{Role: "user", Content: parts})
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "user",
			Content:    "Shared the requested image: " + request.Prompt,
			Images:     []string{filepath.Base(path)},
			OccurredAt: time.Now(),
		})
	}

	prompt := m.requestPrompt
	images := append([]string(nil), m.requestImages...)
	heartbeat := m.requestHeartbeat
	visibleReply := m.requestVisibleReply
	m.resetCaptureState()
	m.phase = phaseChatting
	m.busy = true
	m.err = nil
	m.status = "Continuing with the model…"
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return m.startCompletionStream(append([]apiMessage(nil), m.messages...), prompt, images, heartbeat, visibleReply)
}

func (m *appModel) resetCaptureState() {
	m.imageRequest = nil
	m.requestPrompt = ""
	m.requestImages = nil
	m.requestHeartbeat = false
	m.requestVisibleReply = false
	m.capturePath = ""
	m.captureOwned = false
	m.captureManual = false
	m.previewANSI = ""
	m.previewLoading = false
}

func (m *appModel) discardOwnedCapture() error {
	if m.captureOwned && m.capturePath != "" {
		if err := m.removeOwnedCaptureFile(m.capturePath); err != nil {
			return err
		}
	}
	m.capturePath = ""
	m.captureOwned = false
	return nil
}

func (m *appModel) removeOwnedCaptureFile(path string) error {
	if m.captureVault != nil && m.captureVault.Owns(path) {
		return m.captureVault.Remove(path)
	}
	return removeCaptureFile(path)
}

func removeCaptureFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete camera capture %q: %w", path, err)
	}
	return nil
}

func (m *appModel) consumeOwnedImages(paths []string) error {
	for _, path := range paths {
		if m.captureVault == nil || !m.captureVault.Owns(path) {
			continue
		}
		if err := m.captureVault.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func (m *appModel) clearPendingImages() error {
	err := m.consumeOwnedImages(m.pendingImages)
	m.pendingImages = nil
	return err
}

func (m *appModel) cleanupOwnedImages() error {
	m.stopCameraWindow()
	m.stopCameraPreview(false)
	var cleanupErrors []error
	if m.previewFrame != "" {
		if err := removeCaptureFile(m.previewFrame); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
		m.previewFrame = ""
	}
	if err := m.discardOwnedCapture(); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if m.captureVault != nil {
		if err := m.captureVault.Destroy(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func discardEmbeddedImages(messages []apiMessage) []apiMessage {
	result := append([]apiMessage(nil), messages...)
	for index, message := range result {
		parts, ok := message.Content.([]contentPart)
		if !ok {
			continue
		}
		redacted := make([]contentPart, 0, len(parts))
		for _, part := range parts {
			if part.Type == "image_url" && part.ImageURL != nil {
				redacted = append(redacted, contentPart{
					Type: "text",
					Text: "[Image data discarded after the model processed it.]",
				})
				continue
			}
			redacted = append(redacted, part)
		}
		result[index].Content = redacted
	}
	return result
}

func (m *appModel) submit(value string) tea.Cmd {
	if handled, command := m.handleCommand(value); handled {
		return command
	}

	images := append([]string(nil), m.pendingImages...)
	now := time.Now()
	enveloped, envelopeDebug := m.buildResponseEnvelope(value, now)
	m.lastSent = now
	_ = m.memory.RecordUserActivity(now)

	var content any = enveloped
	if len(images) > 0 {
		parts, err := imagePartsWithVault(enveloped, images, m.captureVault)
		if err != nil {
			m.err = err
			m.status = err.Error()
			return nil
		}
		content = parts
	}
	if err := m.consumeOwnedImages(images); err != nil {
		m.err = err
		m.status = err.Error()
		return nil
	}
	m.pendingImages = nil
	if err := m.memory.AppendRecentMessage("user", value); err != nil {
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "error",
			Content:    "Could not persist recent message: " + err.Error(),
			OccurredAt: now,
		})
	}
	m.messages = append(m.messages, apiMessage{Role: "user", Content: content})
	m.transcript = append(m.transcript, transcriptMessage{
		Role:       "harness",
		Content:    envelopeDebug,
		OccurredAt: now,
		Meta:       "response envelope → model",
	})
	m.transcript = append(m.transcript, transcriptMessage{
		Role:       "user",
		Content:    value,
		Images:     baseNames(images),
		OccurredAt: time.Now(),
	})
	m.busy = true
	m.status = "Sending…"
	m.err = nil
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return tea.Batch(
		m.sendCmd(value, images, false),
		m.armDefaultHeartbeat(now),
	)
}

func (m *appModel) handleCommand(value string) (bool, tea.Cmd) {
	command, argument, _ := strings.Cut(value, " ")
	switch strings.ToLower(command) {
	case "/quit", "/exit":
		m.status = "Closing…"
		return true, nil
	case "/help":
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "notice",
			Content:    "Commands: /whoop · /camera · /image PATH… · /images · /clear-images · /debug [on|off] · /model · /system TEXT · /clear · /help\nKeys: Enter send · Ctrl+J newline · Ctrl+G models · Ctrl+D debug · PgUp/PgDn scroll · Ctrl+C quit",
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
			return true, nil
		}
		m.status = "Debug messages hidden"
		if m.showDebug {
			m.status = "Debug messages visible"
		}
		m.resize()
	case "/camera", "/photo":
		m.beginManualCapture()
	case "/whoop":
		client := newWhoopClient(whoopConfigFromEnvironment(m.memory.dir))
		if client.Configured(time.Now()) {
			m.status = "Whoop is already connected"
			m.transcript = append(m.transcript, transcriptMessage{Role: "notice", Content: "Whoop is connected and recovery/sleep access is available to the assistant.", OccurredAt: time.Now()})
			break
		}
		if client.config.ClientID == "" || client.config.ClientSecret == "" || m.cfg.WhoopRedirectURL == "" {
			m.status = "Whoop setup needs configuration"
			m.transcript = append(m.transcript, transcriptMessage{
				Role: "notice", OccurredAt: time.Now(),
				Content: "To connect Whoop, restart chat2 with WHOOP_CLIENT_ID, WHOOP_CLIENT_SECRET, and WHOOP_REDIRECT_URL set. The redirect URL must be the registered ngrok HTTPS URL ending in /oauth/whoop/callback, then run /whoop again.",
			})
			break
		}
		m.busy = true
		m.status = "Starting ngrok for Whoop…"
		m.composer.Blur()
		cfg := m.cfg
		return true, func() tea.Msg {
			return whoopAuthMsg{err: runWhoopAuthWithNgrok(context.Background(), client, cfg)}
		}
	case "/model":
		m.phase = phasePicking
		m.composer.Blur()
		m.status = "Choose a model · the current transcript will reset"
	case "/clear":
		m.transcript = nil
	case "/images":
		content := "No images queued."
		if len(m.pendingImages) > 0 {
			content = "Queued images:\n- " + strings.Join(m.pendingImages, "\n- ")
		}
		m.transcript = append(m.transcript, transcriptMessage{Role: "notice", Content: content, OccurredAt: time.Now()})
	case "/clear-images":
		count := len(m.pendingImages)
		if err := m.clearPendingImages(); err != nil {
			m.err = err
			m.status = err.Error()
			return true, nil
		}
		m.status = fmt.Sprintf("Cleared %d queued image(s)", count)
	case "/image":
		paths, err := splitArgs(argument)
		if err != nil {
			m.err = err
			m.status = err.Error()
			return true, nil
		}
		if len(paths) == 0 {
			m.status = "Usage: /image PATH [PATH …]"
			return true, nil
		}
		for _, path := range paths {
			resolved, err := resolveImage(path)
			if err != nil {
				m.err = err
				m.status = err.Error()
				return true, nil
			}
			m.pendingImages = append(m.pendingImages, resolved)
		}
		m.status = fmt.Sprintf("%d image(s) queued for the next message", len(m.pendingImages))
	case "/system":
		if strings.TrimSpace(argument) == "" {
			m.status = "Usage: /system TEXT"
			return true, nil
		}
		m.messages = append(m.messages, apiMessage{Role: "system", Content: strings.TrimSpace(argument)})
		m.status = "System context added"
	default:
		return false, nil
	}
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return true, nil
}

func (m *appModel) toggleDebug() {
	m.showDebug = !m.showDebug
	m.status = "Debug messages hidden"
	if m.showDebug {
		m.status = "Debug messages visible"
	}
	m.resize()
	m.viewport.GotoBottom()
}

func (m *appModel) sendCmd(prompt string, images []string, heartbeat bool) tea.Cmd {
	messages := append([]apiMessage(nil), m.messages...)
	if heartbeat {
		messages = append(messages, apiMessage{Role: "system", Content: heartbeatEventPrompt(prompt)})
	}
	return m.startCompletionStream(messages, prompt, images, heartbeat, false)
}

func heartbeatEventPrompt(metadata string) string {
	return heartbeatEventPrefix + `
This is a private control-plane timer event, not a user message and not a new conversation stage. The preceding conversation is unchanged. Use tools only if the metadata gives a concrete reason. If nothing new is due or worth surfacing, immediately call complete_heartbeat with action "none". Otherwise call it with action "notify" and the complete user-facing message. Call complete_heartbeat exactly once and alone; do not answer in prose.

` + strings.TrimSpace(metadata)
}

func (m *appModel) startCompletionStream(messages []apiMessage, prompt string, images []string, heartbeat, visibleReply bool) tea.Cmd {
	m.stopCompletionStream()
	m.streamSession++
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	m.streamCancel = cancel
	m.streamPrompt = prompt
	m.streamImages = append([]string(nil), images...)
	m.streamHeartbeat = heartbeat
	m.streamVisibleReply = visibleReply
	m.streamAnswer = ""
	m.streamReasoning = ""
	m.streamEvents = nil
	m.deliveryStage = deliverySending
	m.deliverySequence++
	m.status = "Sending…"
	session := m.streamSession
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return completionStreamMsg{
				session: session,
				event: completionStreamEvent{
					Kind: streamFinished,
					Err:  errors.New("completion client is not configured"),
				},
				ok: true,
			}
		}
		events := client.completeStream(ctx, messages, !heartbeat)
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
	m.streamPrompt = ""
	m.streamImages = nil
	m.streamHeartbeat = false
	m.streamVisibleReply = false
	m.deliveryStage = deliveryIdle
	m.deliverySequence++
}

func waitCompletionStreamCmd(session uint64, events <-chan completionStreamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		return completionStreamMsg{session: session, events: events, event: event, ok: ok}
	}
}

func deliveryTypingCmd(session, sequence uint64) tea.Cmd {
	return tea.Tick(350*time.Millisecond, func(time.Time) tea.Msg {
		return deliveryTypingMsg{session: session, sequence: sequence}
	})
}

func (m *appModel) startCameraPreview() tea.Cmd {
	m.stopCameraWindow()
	m.stopCameraPreview(true)
	m.previewLoading = true
	m.phase = phaseCameraPreview
	m.status = "Opening camera preview · macOS may ask for permission…"
	return m.cameraPreviewCmd(m.previewSession)
}

func (m *appModel) startCameraWindow() tea.Cmd {
	m.stopCameraPreview(true)
	m.stopCameraWindow()
	m.windowSession++
	ctx, cancel := context.WithCancel(context.Background())
	m.windowCancel = cancel
	m.phase = phaseCameraWindow
	m.status = "Opening full-resolution camera window…"
	session := m.windowSession
	directory := filepath.Join(m.memory.dir, "captures")
	return func() tea.Msg {
		path, cancelled, err := capturePhotoWindow(ctx, directory, m.captureVault, openCameraWindow)
		return cameraWindowResultMsg{session: session, path: path, cancelled: cancelled, err: err}
	}
}

func (m *appModel) stopCameraWindow() {
	if m.windowCancel != nil {
		m.windowCancel()
		m.windowCancel = nil
	}
	m.windowSession++
}

func (m *appModel) stopCameraPreview(removeFrame bool) {
	if m.previewCancel != nil {
		m.previewCancel()
		m.previewCancel = nil
	}
	m.previewSession++
	if removeFrame {
		m.removePreviewFrame()
	}
	m.previewANSI = ""
	m.previewLoading = false
}

func (m *appModel) removePreviewFrame() {
	if m.previewFrame != "" {
		if err := removeCaptureFile(m.previewFrame); err != nil {
			m.err = err
			m.status = err.Error()
			return
		}
	}
	m.previewFrame = ""
	m.previewANSI = ""
}

func (m *appModel) cameraPreviewCmd(session uint64) tea.Cmd {
	directory := filepath.Join(m.memory.dir, "camera-preview")
	width, height := m.previewDimensions()
	ctx, cancel := context.WithCancel(context.Background())
	m.previewCancel = cancel
	return func() tea.Msg {
		captureCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		path, ansi, err := capturePreviewFrame(captureCtx, directory, width, height)
		return cameraPreviewMsg{session: session, path: path, ansi: ansi, err: err}
	}
}

func cameraBackendAvailable() bool {
	_, err := exec.LookPath("imagesnap")
	return err == nil
}

func capturePreviewFrame(ctx context.Context, directory string, width, height int) (string, string, error) {
	backend, err := exec.LookPath("imagesnap")
	if err != nil {
		return "", "", fmt.Errorf("ImageSnap is required for camera preview; install it with: brew install imagesnap")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", fmt.Errorf("create preview directory: %w", err)
	}
	path := filepath.Join(directory, "preview-"+time.Now().Format("20060102-150405.000000000")+".jpg")
	command := exec.CommandContext(ctx, backend, "-q", "-w", "0.20", path)
	output, err := command.CombinedOutput()
	if err != nil {
		cleanupErr := removeCaptureFile(path)
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", "", errors.Join(fmt.Errorf("camera preview failed: %s", message), cleanupErr)
	}
	ansi, err := renderImageFile(path, width, height)
	if err != nil {
		return "", "", errors.Join(fmt.Errorf("render camera preview: %w", err), removeCaptureFile(path))
	}
	return path, ansi, nil
}

func (m *appModel) keepPreviewFrame() (string, error) {
	if m.previewFrame == "" {
		return "", fmt.Errorf("no camera frame is ready")
	}
	if m.captureVault == nil {
		return "", errors.New("encrypted capture vault is unavailable")
	}
	plaintext, err := os.ReadFile(m.previewFrame)
	if err != nil {
		return "", fmt.Errorf("read camera preview: %w", err)
	}
	defer clear(plaintext)
	directory := filepath.Join(m.memory.dir, "captures")
	destination, err := m.captureVault.SealJPEG(directory, plaintext)
	if err != nil {
		return "", err
	}
	if err := removeCaptureFile(m.previewFrame); err != nil {
		_ = m.captureVault.Remove(destination)
		return "", err
	}
	m.previewFrame = ""
	m.stopCameraPreview(false)
	return destination, nil
}

func renderImageFile(path string, width, height int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	frame, err := jpeg.Decode(file)
	if err != nil {
		return "", err
	}
	return renderImageANSI(frame, width, height), nil
}

func renderImageANSI(frame image.Image, maxWidth, maxRows int) string {
	bounds := frame.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || maxWidth <= 0 || maxRows <= 0 {
		return ""
	}
	columns := min(maxWidth, bounds.Dx())
	rows := max(1, int(math.Round(float64(columns)*float64(bounds.Dy())/float64(bounds.Dx())/2)))
	if rows > maxRows {
		rows = maxRows
		columns = max(1, int(math.Round(float64(rows)*2*float64(bounds.Dx())/float64(bounds.Dy()))))
	}

	var result strings.Builder
	result.Grow(columns * rows * 36)
	for row := range rows {
		for column := range columns {
			x := bounds.Min.X + (2*column+1)*bounds.Dx()/(2*columns)
			topY := bounds.Min.Y + (4*row+1)*bounds.Dy()/(4*rows)
			bottomY := bounds.Min.Y + (4*row+3)*bounds.Dy()/(4*rows)
			topR, topG, topB, _ := frame.At(x, topY).RGBA()
			bottomR, bottomG, bottomB, _ := frame.At(x, bottomY).RGBA()
			fmt.Fprintf(&result, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", topR>>8, topG>>8, topB>>8, bottomR>>8, bottomG>>8, bottomB>>8)
		}
		result.WriteString("\x1b[0m")
		if row < rows-1 {
			result.WriteByte('\n')
		}
	}
	return result.String()
}

func cameraPreviewTickCmd(session uint64) tea.Cmd {
	return tea.Tick(350*time.Millisecond, func(time.Time) tea.Msg {
		return cameraPreviewTickMsg{session: session}
	})
}

func (m appModel) previewDimensions() (int, int) {
	return max(8, min(80, m.width-14)), max(4, m.height-12)
}

func (m *appModel) armHeartbeatAfterLastUser(now time.Time) tea.Cmd {
	due := now.Add(m.cfg.Heartbeat)
	if !m.lastSent.IsZero() {
		due = m.lastSent.Add(m.cfg.Heartbeat)
		if due.Before(now) {
			due = now
		}
	}
	return m.armHeartbeatAt(due)
}

func (m *appModel) armDefaultHeartbeat(now time.Time) tea.Cmd {
	return m.armHeartbeatAt(now.Add(m.cfg.Heartbeat))
}

func (m *appModel) armHeartbeatAt(due time.Time) tea.Cmd {
	return m.armHeartbeat(due, false)
}

func (m *appModel) armExplicitHeartbeatAt(due time.Time) tea.Cmd {
	return m.armHeartbeat(due, true)
}

func (m *appModel) armHeartbeat(due time.Time, explicitlyScheduled bool) tea.Cmd {
	if m.cfg.Heartbeat <= 0 {
		m.disarmHeartbeat()
		return nil
	}
	if due.Before(time.Now()) {
		due = time.Now()
	}
	m.heartbeatGeneration++
	m.heartbeatArmed = true
	m.heartbeatDue = due
	m.heartbeatExplicit = explicitlyScheduled
	return heartbeatCmd(due, m.heartbeatGeneration)
}

func (m *appModel) disarmHeartbeat() {
	m.heartbeatGeneration++
	m.heartbeatArmed = false
	m.heartbeatDue = time.Time{}
	m.heartbeatExplicit = false
}

func heartbeatCmd(due time.Time, generation uint64) tea.Cmd {
	delay := time.Until(due)
	if delay < 0 {
		delay = 0
	}
	return tea.Tick(delay, func(now time.Time) tea.Msg {
		return heartbeatMsg{generation: generation, at: now}
	})
}

func (m *appModel) resize() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	bodyWidth := max(24, m.width-4)
	m.composer.SetWidth(bodyWidth - 4)
	composerHeight := 6
	debugStatusHeight := 0
	if m.showDebug {
		debugStatusHeight = 1
	}
	viewportHeight := max(4, m.height-composerHeight-6-debugStatusHeight)
	m.viewport.SetWidth(bodyWidth)
	m.viewport.SetHeight(viewportHeight)
	m.filePicker.SetHeight(max(5, m.height-12))
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
	visibleStreamAnswer := hidePotentialTrailingHeartbeatDirective(m.streamAnswer)
	if visibleStreamAnswer != "" {
		messages = append(messages, transcriptMessage{
			Role:       "assistant_stream",
			Content:    visibleStreamAnswer,
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
	case phaseStarting:
		content = m.renderStarting()
	case phaseChatting:
		content = m.renderChat()
	case phaseImageRequest:
		content = m.renderImageRequest()
	case phaseCameraWindow:
		content = m.renderCameraWindow()
	case phaseCameraPreview:
		content = m.renderCameraPreview()
	case phaseFilePicking:
		content = m.renderFilePicker()
	case phasePhotoReview:
		content = m.renderPhotoReview()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	// Cell-motion mode includes wheel events without requesting continuous mouse
	// movement reports. The chat viewport consumes those events for history
	// scrolling while keyboard focus remains in the composer.
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func (m appModel) renderStarting() string {
	style := lipgloss.NewStyle().Foreground(m.palette.text).Padding(2, 4)
	message := m.spinner.View() + " communicating with remote..."
	if m.err != nil {
		message = lipgloss.NewStyle().Foreground(m.palette.danger).Render(m.err.Error()) +
			"\n\n" + lipgloss.NewStyle().Foreground(m.palette.muted).Render("r retry · esc models · Ctrl+C quit")
	}
	return style.Render(message)
}

func (m appModel) renderCameraWindow() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Camera window open")
	body := lipgloss.NewStyle().Foreground(m.palette.text).Render(
		"Use the full-resolution viewfinder in your browser. Capture, retake, then choose Use Photo to return it here.",
	)
	privacy := lipgloss.NewStyle().Foreground(m.palette.accent).Render(
		"The page is served only on 127.0.0.1 with a one-time private URL. Nothing reaches the model yet.",
	)
	controls := lipgloss.NewStyle().Foreground(m.palette.secondary).Render("esc/q cancel and close the local capture session")
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.accent).
		Padding(1, 2).
		Width(max(40, min(m.width-8, 92))).
		Render(title + "\n\n" + body + "\n\n" + privacy + "\n\n" + controls + "\n\n" + m.renderStatus())
	return lipgloss.NewStyle().Padding(2, 4).Render(panel)
}

func (m appModel) renderCameraPreview() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Camera preview")
	frame := m.previewANSI
	if frame == "" {
		frame = m.spinner.View() + " Waiting for the first frame…"
	}
	controls := lipgloss.NewStyle().Foreground(m.palette.secondary).Render(
		"Space/Enter capture this frame · r refresh · f choose a file · esc cancel",
	)
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.accent).
		Padding(1, 2).
		Width(max(30, min(m.width-8, 92))).
		Render(title + "\n\n" + frame + "\n\n" + controls + "\n" + m.renderStatus())
	return lipgloss.NewStyle().Padding(1, 3).Render(panel)
}

func (m appModel) renderDiscovering() string {
	style := lipgloss.NewStyle().Foreground(m.palette.text).Padding(2, 4)
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("gemma4 · chat2")
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
	header := m.renderHeader()
	status := m.renderStatus()
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
	footer := lipgloss.NewStyle().
		Foreground(m.palette.muted).
		Render("Enter send  ·  Ctrl+J newline  ·  Ctrl+G models  ·  Ctrl+D debug  ·  /camera capture  ·  Ctrl+C quit")
	return lipgloss.JoinVertical(lipgloss.Left, header, m.viewport.View(), status, composer, footer)
}

func (m appModel) renderImageRequest() string {
	request := m.imageRequest
	prompt := "Take or choose a photo."
	reason := "You opened the photo tool."
	title := "Photo capture"
	if request != nil {
		prompt = request.Prompt
		reason = request.Reason
	}
	if !m.captureManual {
		title = "The model is requesting an image"
	}
	cameraState := lipgloss.NewStyle().Foreground(m.palette.success).Render("browser window available")
	terminalState := lipgloss.NewStyle().Foreground(m.palette.success).Render("ImageSnap installed")
	if !m.cfg.AllowPlaintextPreview {
		terminalState = lipgloss.NewStyle().Foreground(m.palette.muted).Render("disabled for encrypted-at-rest mode")
	} else if !m.cameraAvailable {
		terminalState = lipgloss.NewStyle().Foreground(m.palette.danger).Render("ImageSnap not installed")
	}
	body := lipgloss.NewStyle().Foreground(m.palette.text).Render(prompt)
	why := lipgloss.NewStyle().Foreground(m.palette.muted).Render("Why: " + reason)
	privacy := lipgloss.NewStyle().Foreground(m.palette.accent).Render(
		"Nothing is captured or shared until you choose it. Camera access requires your explicit keypress and macOS permission.",
	)
	controls := "c  open full camera window  ·  t  terminal preview  ·  f  choose an image  ·  d/esc  decline"
	if m.captureManual {
		controls = "c  open full camera window  ·  t  terminal preview  ·  f  choose an image  ·  d/esc  cancel"
	}
	content := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.accent).
		Padding(1, 2).
		Width(max(40, min(m.width-8, 92))).
		Render(
			lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render(title) +
				"\n\n" + body + "\n\n" + why + "\n\n" + privacy +
				"\n\nCamera: " + cameraState + " · terminal fallback: " + terminalState + "\n\n" +
				lipgloss.NewStyle().Foreground(m.palette.secondary).Render(controls) +
				"\n\n" + m.renderStatus(),
		)
	return lipgloss.NewStyle().Padding(2, 4).Render(content)
}

func (m appModel) renderFilePicker() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Choose an image")
	location := lipgloss.NewStyle().Foreground(m.palette.muted).Render(m.filePicker.CurrentDirectory)
	help := lipgloss.NewStyle().Foreground(m.palette.secondary).Render("↑/↓ move · enter open/select · ← parent · q cancel")
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.primary).
		Padding(1, 2).
		Width(max(40, min(m.width-8, 100))).
		Render(title + "\n" + location + "\n\n" + m.filePicker.View() + "\n" + help)
	return lipgloss.NewStyle().Padding(1, 3).Render(panel)
}

func (m appModel) renderPhotoReview() string {
	title := "Review captured photo"
	if !m.captureManual {
		title = "Review before sharing with the model"
	}
	info, _ := os.Stat(m.capturePath)
	detail := m.capturePath
	if info != nil {
		detail += fmt.Sprintf("\n%.1f KB · captured %s", float64(info.Size())/1024, info.ModTime().Format("15:04:05"))
	}
	verb := "queue"
	if !m.captureManual {
		verb = "share"
	}
	controls := fmt.Sprintf("s/enter  %s photo  ·  r  retake  ·  f  choose another  ·  d/esc  discard", verb)
	discardNotice := "Camera captures are deleted after encoding or discard. Selected files are never deleted."
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.success).
		Padding(1, 2).
		Width(max(40, min(m.width-8, 92))).
		Render(
			lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render(title) +
				"\n\n" + lipgloss.NewStyle().Foreground(m.palette.text).Render(detail) +
				"\n\n" + lipgloss.NewStyle().Foreground(m.palette.accent).Render("The image has not been sent.") +
				"\n" + lipgloss.NewStyle().Foreground(m.palette.muted).Render(discardNotice) +
				"\n\n" + lipgloss.NewStyle().Foreground(m.palette.secondary).Render(controls),
		)
	return lipgloss.NewStyle().Padding(2, 4).Render(panel)
}

func (m appModel) renderHeader() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("gemma4")
	model := lipgloss.NewStyle().Bold(true).Foreground(m.palette.text).Render(m.selected.Model)
	backend := lipgloss.NewStyle().Foreground(m.palette.secondary).Render(string(m.selected.Backend))
	mtag := ""
	if m.selected.Backend == backendLlama {
		mtag = "  " + lipgloss.NewStyle().Foreground(m.palette.accent).Render("VISION · MTP")
	} else {
		mtag = "  " + lipgloss.NewStyle().Foreground(m.palette.accent).Render("VISION")
	}
	contextPct := 0
	if m.cfg.Context > 0 {
		contextPct = int(math.Min(100, float64(m.promptTokens)/float64(m.cfg.Context)*100))
	}
	right := fmt.Sprintf("%d%% ctx  ·  %d memories", contextPct, m.memoryCount)
	if len(m.pendingImages) > 0 {
		right += fmt.Sprintf("  ·  %d image(s)", len(m.pendingImages))
	}
	if m.showDebug {
		right += "  ·  DEBUG"
	}
	left := title + "  " + model + "  " + backend + mtag
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
		switch m.deliveryStage {
		case deliverySending:
			text = "Sending…"
			color = m.palette.secondary
		case deliveryDelivered:
			text = "Sending ✓  ›  Delivered ✓"
			color = m.palette.success
		case deliveryTyping:
			text = "Sending ✓  ›  Delivered ✓  ›  Typing…"
			color = m.palette.accent
		default:
			text = m.spinner.View() + " " + text
			color = m.palette.accent
		}
	}
	if m.err != nil {
		color = m.palette.danger
	}
	if m.showDebug {
		text += "\n" + m.heartbeatDebugStatus()
	}
	return lipgloss.NewStyle().Foreground(color).Padding(0, 1).Render(text)
}

func (m appModel) heartbeatDebugStatus() string {
	if m.cfg.Heartbeat <= 0 {
		return "Next heartbeat: disabled"
	}
	if !m.heartbeatArmed || m.heartbeatDue.IsZero() {
		return "Next heartbeat: none"
	}
	status := "Next heartbeat: " + m.heartbeatDue.Local().Format("3:04 PM")
	if m.heartbeatExplicit {
		status += " [scheduled]"
	}
	return status
}

func (m appModel) renderTranscript(width int) string {
	return m.renderTranscriptMessages(m.transcript, width)
}

func recentMessagesTranscript(messages []recentMessage) []transcriptMessage {
	result := make([]transcriptMessage, 0, len(messages))
	for _, message := range messages {
		occurredAt, err := time.Parse(time.RFC3339Nano, message.Timestamp)
		if err != nil {
			occurredAt = time.Now()
		}
		result = append(result, transcriptMessage{
			Role:       message.Role,
			Content:    message.Content,
			OccurredAt: occurredAt,
			Meta:       "restored",
		})
	}
	return result
}

func (m appModel) renderTranscriptMessages(messages []transcriptMessage, width int) string {
	var blocks []string
	for _, message := range messages {
		if (message.Role == "system" || message.Role == "reasoning" || message.Role == "harness") && !m.showDebug {
			continue
		}
		label := strings.ToUpper(message.Role)
		color := m.palette.muted
		switch message.Role {
		case "user":
			color = m.palette.secondary
			if m.userLabel != "" {
				label = m.userLabel
			}
		case "assistant":
			color = m.palette.primary
			if m.assistantLabel != "" {
				label = m.assistantLabel
			}
		case "assistant_stream":
			color = m.palette.primary
			label = m.assistantLabel
			if label == "" {
				label = "ASSISTANT"
			}
		case "reasoning":
			color = m.palette.accent
			label = m.assistantLabel
			if label == "" {
				label = "ASSISTANT"
			}
			label += " · THINKING"
		case "harness":
			color = m.palette.accent
			label = "HARNESS"
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

func startupDebugMessages(initial, completed []apiMessage) []apiMessage {
	debug := make([]apiMessage, 0, 1+max(0, len(completed)-len(initial)))
	for _, message := range initial {
		content, ok := message.Content.(string)
		if ok && content == harnessProtocolInstructions {
			debug = append(debug, message)
			break
		}
	}
	if len(completed) > len(initial) {
		debug = append(debug, completed[len(initial):]...)
	}
	return debug
}

func startupDebugTranscript(messages []apiMessage, assistantLabel string) []transcriptMessage {
	result := make([]transcriptMessage, 0, len(messages))
	for _, message := range messages {
		meta := "startup protocol"
		switch message.Role {
		case "system":
			meta += " · harness → model"
		case "assistant":
			modelLabel := assistantLabel
			if modelLabel == "" {
				modelLabel = "model"
			}
			meta += " · " + modelLabel + " → harness"
		case "tool":
			meta += " · harness tool result"
		default:
			meta += " · " + message.Role
		}
		result = append(result, transcriptMessage{
			Role:       "harness",
			Content:    formatStartupDebugMessage(message),
			OccurredAt: time.Now(),
			Meta:       meta,
		})
	}
	return result
}

func formatStartupDebugMessage(message apiMessage) string {
	var sections []string
	if message.ReasoningContent != "" {
		sections = append(sections, "Reasoning:\n"+message.ReasoningContent)
	}
	if message.Content != nil {
		if content, ok := message.Content.(string); ok {
			if content != "" {
				sections = append(sections, content)
			}
		} else if encoded, err := json.MarshalIndent(message.Content, "", "  "); err == nil {
			sections = append(sections, string(encoded))
		} else {
			sections = append(sections, fmt.Sprint(message.Content))
		}
	}
	for _, call := range message.ToolCalls {
		sections = append(sections, fmt.Sprintf(
			"Tool call: %s\nID: %s\nArguments: %s",
			call.Function.Name,
			call.ID,
			call.Function.Arguments,
		))
	}
	if message.ToolCallID != "" {
		sections = append(sections, "Tool call ID: "+message.ToolCallID)
	}
	if len(sections) == 0 {
		return "(empty message)"
	}
	return strings.Join(sections, "\n\n")
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

type responseEnvelopeMetadata struct {
	SentAt               string                    `json:"sent_at"`
	SinceLastUserMessage string                    `json:"since_last_user_message"`
	Schedule             *responseEnvelopeSchedule `json:"schedule,omitempty"`
	ScheduleError        string                    `json:"schedule_error,omitempty"`
}

type responseEnvelopeSchedule struct {
	UpcomingThrough   string             `json:"upcoming_through"`
	Due               []scheduleItemView `json:"due,omitempty"`
	Upcoming          []scheduleItemView `json:"upcoming,omitempty"`
	NextLater         *scheduleItemView  `json:"next_later,omitempty"`
	RecentlyCompleted []scheduleItemView `json:"recently_completed,omitempty"`
}

func (m *appModel) responseEnvelope(text string, now time.Time) string {
	enveloped, _ := m.buildResponseEnvelope(text, now)
	return enveloped
}

func (m *appModel) buildResponseEnvelope(text string, now time.Time) (string, string) {
	metadata := responseEnvelopeMetadata{
		SentAt:               humanTimestamp(now, now.Location()),
		SinceLastUserMessage: timeSinceUser(now, m.lastSent),
	}
	if m.schedule != nil {
		snapshot, err := m.schedule.Snapshot(
			now,
			defaultScheduleUpcomingWindow,
			defaultScheduleHistoryWindow,
		)
		if err != nil {
			metadata.ScheduleError = err.Error()
		} else {
			unseenCompleted := make([]scheduleEntry, 0, len(snapshot.RecentCompleted))
			for _, entry := range snapshot.RecentCompleted {
				if _, seen := m.seenCompletions[entry.ID]; !seen {
					unseenCompleted = append(unseenCompleted, entry)
				}
			}
			if len(snapshot.Due)+len(snapshot.Upcoming)+len(snapshot.Later)+len(unseenCompleted) > 0 {
				later := humanScheduleViews(snapshot.Later, now)
				metadata.Schedule = &responseEnvelopeSchedule{
					UpcomingThrough:   humanTimestamp(now.Add(defaultScheduleUpcomingWindow), now.Location()),
					Due:               humanScheduleViews(snapshot.Due, now),
					Upcoming:          humanScheduleViews(snapshot.Upcoming, now),
					RecentlyCompleted: humanScheduleViews(unseenCompleted, now),
				}
				if len(later) > 0 {
					metadata.Schedule.NextLater = &later[0]
				}
				if m.seenCompletions == nil {
					m.seenCompletions = make(map[int]struct{})
				}
				for _, entry := range unseenCompleted {
					m.seenCompletions[entry.ID] = struct{}{}
				}
			}
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		encoded = []byte(fmt.Sprintf(`{"sent_at":%q,"since_last_user_message":%q}`, metadata.SentAt, metadata.SinceLastUserMessage))
	}
	metadataJSON := string(encoded)
	return "[HARNESS METADATA]\n" + metadataJSON + "\n[/HARNESS METADATA]\n" + text, metadataJSON
}

func humanTimestamp(value time.Time, location *time.Location) string {
	return value.In(location).Format("Monday January 2, 3:04 PM MST")
}

func humanScheduleViews(entries []scheduleEntry, now time.Time) []scheduleItemView {
	views := scheduleViews(entries, now)
	for index, entry := range entries {
		views[index].Time = humanTimestamp(entry.Time, now.Location())
		if entry.CompletedAt != nil {
			views[index].CompletedAt = humanTimestamp(*entry.CompletedAt, now.Location())
		}
	}
	return views
}

func timeSinceUser(now, previous time.Time) string {
	if previous.IsZero() {
		return "no previous user message"
	}
	elapsed := now.Sub(previous).Round(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.String()
}

func baseNames(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		result = append(result, filepath.Base(path))
	}
	return result
}
