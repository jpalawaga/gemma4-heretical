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
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
	phaseImageManager
	phaseScheduleManager
	phaseScheduleAction
	phaseCompacting
	phaseSettings
	phaseCategoryEditor
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
	cfg                        appConfig
	phase                      phase
	width                      int
	height                     int
	choices                    []modelChoice
	cursor                     int
	selected                   modelChoice
	viewport                   viewport.Model
	composer                   textarea.Model
	filePicker                 filepicker.Model
	spinner                    spinner.Model
	transcript                 []transcriptMessage
	restoredTranscript         []transcriptMessage
	messages                   []apiMessage
	pendingImages              []string
	imageManagerCursor         int
	returnToImageManager       bool
	scheduleManagerEntries     []scheduleEntry
	scheduleManagerCursor      int
	scheduleManagerSelected    map[int]struct{}
	scheduleActionCursor       int
	scheduleComment            textarea.Model
	captureVault               *captureVault
	client                     *apiClient
	memory                     *memoryStore
	schedule                   *scheduleStore
	memoryCount                int
	busy                       bool
	status                     string
	err                        error
	promptTokens               int
	lastSent                   time.Time
	palette                    palette
	imageRequest               *imageRequest
	requestPrompt              string
	requestImages              []string
	requestHeartbeat           bool
	requestVisibleReply        bool
	capturePath                string
	captureOwned               bool
	captureManual              bool
	cameraAvailable            bool
	previewFrame               string
	previewANSI                string
	previewSession             uint64
	previewLoading             bool
	previewCancel              context.CancelFunc
	windowSession              uint64
	windowCancel               context.CancelFunc
	startupSession             uint64
	startupCancel              context.CancelFunc
	userLabel                  string
	assistantLabel             string
	showDebug                  bool
	compactTranscript          bool
	streamSession              uint64
	streamCancel               context.CancelFunc
	streamEvents               <-chan completionStreamEvent
	streamAnswer               string
	streamReasoning            string
	streamPrompt               string
	streamImages               []string
	streamHeartbeat            bool
	streamVisibleReply         bool
	seenCompletions            map[int]struct{}
	pendingScheduleCompletions map[int]scheduleEntry
	pendingScheduleRequests    []scheduleCompletionRequest
	deliveryStage              deliveryStage
	deliverySequence           uint64
	heartbeatGeneration        uint64
	heartbeatArmed             bool
	heartbeatDue               time.Time
	heartbeatExplicit          bool
	compactedLoaded            bool
	sessionReplies             int
	compactSession             uint64
	compactCancel              context.CancelFunc
	compactEvents              <-chan completionStreamEvent
	compactText                string
	compactLog                 []string
	compactMidSession          bool
	startupMessageCount        int
	exitNotice                 string
	settingsDraft              settings
	settingsFields             []settingField
	settingsCursor             int
	settingsEditing            bool
	settingsInput              textinput.Model
	settingsError              string
	settingsReturn             phase
	settingsArea               textarea.Model
	categoriesDraft            []memoryCategory
	categoriesCursor           int
	categoriesEditing          bool
	categoriesAdding           bool
	categoriesInput            textinput.Model
	categoriesError            string
	categoriesCounts           map[string]int
	autoSelected               bool
}

type scheduleCompletionRequest struct {
	Entry   scheduleEntry
	Comment string
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

type compactStreamMsg struct {
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
	composer.Placeholder = "Ask anything…  Enter sends · Shift+Enter adds a line"
	composer.ShowLineNumbers = false
	composer.SetHeight(4)
	composer.CharLimit = 32_000

	scheduleComment := textarea.New()
	scheduleComment.Placeholder = "Required completion comment…"
	scheduleComment.ShowLineNumbers = false
	scheduleComment.SetHeight(3)
	scheduleComment.CharLimit = 4_000

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
		cfg:                        cfg,
		phase:                      phaseDiscovering,
		viewport:                   viewport.New(),
		composer:                   composer,
		scheduleComment:            scheduleComment,
		filePicker:                 picker,
		spinner:                    spin,
		memory:                     memory,
		schedule:                   newScheduleStore(memory.dir),
		pendingImages:              append([]string(nil), cfg.StartupImages...),
		captureVault:               newCaptureVault(),
		lastSent:                   memory.LastUserActivityTime(),
		status:                     "Looking for local models…",
		palette:                    colors,
		cameraAvailable:            cfg.AllowPlaintextPreview && cameraBackendAvailable(),
		userLabel:                  "USER",
		assistantLabel:             "ASSISTANT",
		showDebug:                  cfg.Debug,
		compactTranscript:          cfg.CompactTranscript,
		seenCompletions:            make(map[int]struct{}),
		pendingScheduleCompletions: make(map[int]scheduleEntry),
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
		if m.cfg.PreferredModel != "" && !m.autoSelected {
			m.autoSelected = true
			if choice, ok := m.preferredChoice(); ok {
				cmd, err := m.selectModel(choice)
				if err != nil {
					m.err = err
					m.status = err.Error()
				} else {
					commands = append(commands, cmd)
					return m, tea.Batch(commands...)
				}
			} else {
				m.status = "Preferred model " + m.cfg.PreferredModel + " is offline · choose a model"
			}
		}
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
			m.startupMessageCount = len(msg.result.Messages)
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
				Meta:       m.startupContextSummary(),
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
			followStream := m.viewport.AtBottom()
			m.streamReasoning += msg.event.Delta
			if m.deliveryStage != deliveryDelivered {
				m.deliveryStage = deliveryTyping
				m.status = "Typing…"
			}
			m.refreshTranscript()
			if followStream {
				m.viewport.GotoBottom()
			}
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamContentDelta:
			followStream := m.viewport.AtBottom()
			m.streamAnswer += msg.event.Delta
			if m.deliveryStage != deliveryDelivered {
				m.deliveryStage = deliveryTyping
				m.status = "Typing…"
			}
			m.refreshTranscript()
			if followStream {
				m.viewport.GotoBottom()
			}
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamToolRound:
			followStream := m.viewport.AtBottom()
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
			if followStream {
				m.viewport.GotoBottom()
			}
			commands = append(commands, waitCompletionStreamCmd(msg.session, m.streamEvents))
		case streamNotice:
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
	case compactStreamMsg:
		if msg.session != m.compactSession || m.phase != phaseCompacting {
			return m, tea.Batch(commands...)
		}
		if m.compactEvents == nil {
			m.compactEvents = msg.events
		}
		if !msg.ok {
			m.finishCompaction(completionResult{}, errors.New("compaction stream closed unexpectedly"))
			return m, tea.Quit
		}
		switch msg.event.Kind {
		case streamConnected:
			if len(m.compactLog) == 0 {
				m.status = "Reviewing the session…"
			}
			commands = append(commands, waitCompactStreamCmd(msg.session, m.compactEvents))
		case streamToolRound:
			m.compactLog = append(m.compactLog, "› "+strings.Join(msg.event.ToolNames, ", "))
			m.status = fmt.Sprintf("Round %d · %s", len(m.compactLog), strings.Join(msg.event.ToolNames, ", "))
			commands = append(commands, waitCompactStreamCmd(msg.session, m.compactEvents))
		case streamNotice:
			m.compactLog = append(m.compactLog, "! "+msg.event.Delta)
			m.status = "Retrying with a shorter history…"
			commands = append(commands, waitCompactStreamCmd(msg.session, m.compactEvents))
		case streamContentDelta:
			m.compactText += msg.event.Delta
			commands = append(commands, waitCompactStreamCmd(msg.session, m.compactEvents))
		case streamFinished:
			if m.compactMidSession {
				commands = append(commands, m.finishMidSessionCompaction(msg.event.Result, msg.event.Err))
				return m, tea.Batch(commands...)
			}
			m.finishCompaction(msg.event.Result, msg.event.Err)
			return m, tea.Quit
		default:
			commands = append(commands, waitCompactStreamCmd(msg.session, m.compactEvents))
		}
	case deliveryTypingMsg:
		if msg.session == m.streamSession && msg.sequence == m.deliverySequence && m.phase == phaseChatting && m.busy && m.deliveryStage == deliveryDelivered {
			m.deliveryStage = deliveryTyping
			m.status = "Typing…"
		}
	case responseMsg:
		followTranscript := m.viewport.AtBottom()
		m.busy = false
		mutationChips, remainingNotices := mutationNoticeChips(msg.result.Notices)
		if msg.err == nil && msg.result.ImageRequest != nil {
			if len(mutationChips) > 0 {
				m.transcript = append(m.transcript, transcriptMessage{
					Role:       "notice",
					Chips:      mutationChips,
					OccurredAt: time.Now(),
					Meta:       strings.Join(remainingNotices, " · "),
				})
			}
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
			if msg.err == nil && len(mutationChips) > 0 {
				m.transcript = append(m.transcript, transcriptMessage{
					Role:       "notice",
					Chips:      mutationChips,
					OccurredAt: time.Now(),
					Meta:       "heartbeat",
				})
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
				if followTranscript {
					m.viewport.GotoBottom()
				}
			}
			acted := msg.err == nil && (msg.result.HadVisibleReply || strings.TrimSpace(msg.result.Reply) != "")
			if !acted && m.cfg.Heartbeat > 0 {
				commands = append(commands, m.armDefaultHeartbeat(time.Now()))
			}
			m.refreshTranscript()
			if followTranscript {
				m.viewport.GotoBottom()
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
				Chips:      mutationChips,
				OccurredAt: time.Now(),
				Meta:       strings.Join(remainingNotices, " · "),
			})
			m.status = "Ready"
			if m.cfg.Context > 0 && m.promptTokens*100 >= m.cfg.Context*85 {
				m.status = fmt.Sprintf("Ready · context %d%% full · /compact recommended", m.promptTokens*100/m.cfg.Context)
			}
			m.sessionReplies++
			if err := m.memory.AppendRecentMessage("assistant", msg.result.Reply); err != nil {
				m.status = "Ready · could not persist recent response"
			}
		}
		m.refreshTranscript()
		if followTranscript {
			m.viewport.GotoBottom()
		}
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
			if m.returnToImageManager {
				m.finishImageRequest("", true)
			} else {
				m.phase = phaseImageRequest
				m.status = "Camera window cancelled · no photo was kept"
			}
		} else {
			m.err = nil
			m.capturePath = msg.path
			m.captureOwned = true
			cmd := m.finishImageRequest(msg.path, false)
			if cmd != nil {
				commands = append(commands, cmd)
			}
			return m, tea.Batch(commands...)
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
	case phaseImageManager:
		return m.updateImageManager(msg, commands)
	case phaseScheduleManager:
		return m.updateScheduleManager(msg, commands)
	case phaseScheduleAction:
		return m.updateScheduleAction(msg, commands)
	case phaseCompacting:
		return m.updateCompacting(msg, commands)
	case phaseSettings:
		return m.updateSettings(msg, commands)
	case phaseCategoryEditor:
		return m.updateCategoryEditor(msg, commands)
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
	case "f2", "ctrl+,":
		m.openSettings()
		return m, tea.Batch(commands...)
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
	m.client.exposeAllTools = !m.cfg.LeanTools
	if m.cfg.LeanTools {
		whoopConnected := newWhoopClient(whoopConfigFromEnvironment(m.memory.dir)).Configured(time.Now())
		initial = append(initial, apiMessage{Role: "system", Content: leanToolsInstructions(whoopConnected)})
	}
	m.phase = phaseStarting
	m.transcript = nil
	m.userLabel = "USER"
	m.assistantLabel = "ASSISTANT"
	m.seenCompletions = make(map[int]struct{})
	m.sessionReplies = 0
	_, m.compactedLoaded, _ = m.memory.CompactedConversation()
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
			return m.beginExit()
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
		case "f2", "ctrl+,":
			if !m.busy {
				m.openSettings()
			}
			return m, tea.Batch(commands...)
		case "ctrl+i":
			if !m.busy {
				m.openImageManager()
			}
			return m, tea.Batch(commands...)
		case "ctrl+s":
			if !m.busy {
				m.openScheduleManager(time.Now())
			}
			return m, tea.Batch(commands...)
		case "ctrl+o":
			m.toggleCompactTranscript()
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
					m.status = "Draft saved · wait for the agent to finish, then press Enter"
				}
				return m, tea.Batch(commands...)
			}
			value := strings.TrimSpace(m.composer.Value())
			if value != "" {
				m.composer.Reset()
				if value == "/quit" || value == "/exit" {
					return m.beginExit()
				}
				cmd := m.submit(value)
				if cmd != nil {
					commands = append(commands, cmd)
				}
			}
			return m, tea.Batch(commands...)
		}
	}

	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	commands = append(commands, cmd)
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
		if m.returnToImageManager {
			m.finishImageRequest("", true)
		} else {
			m.phase = phaseImageRequest
			m.status = "Camera window cancelled · no photo was kept"
		}
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
		if m.returnToImageManager {
			m.finishImageRequest("", true)
		} else {
			m.phase = phaseImageRequest
			m.status = "Camera preview cancelled · no photo was kept"
		}
	}
	return m, tea.Batch(commands...)
}

func (m appModel) updateFilePicker(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if m.returnToImageManager {
				m.finishImageRequest("", true)
			} else {
				m.phase = phaseImageRequest
				m.status = "Image selection cancelled"
			}
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

func (m *appModel) openImageManager() {
	m.phase = phaseImageManager
	m.composer.Blur()
	m.err = nil
	if len(m.pendingImages) == 0 {
		m.imageManagerCursor = 0
		m.status = "Image queue is empty"
		return
	}
	if m.imageManagerCursor >= len(m.pendingImages) {
		m.imageManagerCursor = len(m.pendingImages) - 1
	}
	m.status = queuedImagesSummary(len(m.pendingImages))
}

func (m appModel) updateImageManager(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+i", "esc", "q":
		m.phase = phaseChatting
		m.status = "Ready"
		commands = append(commands, m.composer.Focus())
	case "up", "k":
		if m.imageManagerCursor > 0 {
			m.imageManagerCursor--
		}
	case "down", "j":
		if m.imageManagerCursor+1 < len(m.pendingImages) {
			m.imageManagerCursor++
		}
	case "d", "x", "backspace", "delete":
		if len(m.pendingImages) == 0 {
			m.status = "Image queue is already empty"
			break
		}
		removed := filepath.Base(m.pendingImages[m.imageManagerCursor])
		if err := m.removeQueuedImage(m.imageManagerCursor); err != nil {
			m.err = err
			m.status = err.Error()
			break
		}
		m.status = fmt.Sprintf("Removed %s · %s", removed, queuedImagesSummary(len(m.pendingImages)))
	case "ctrl+x":
		count := len(m.pendingImages)
		if err := m.clearPendingImages(); err != nil {
			m.err = err
			m.status = err.Error()
			break
		}
		m.imageManagerCursor = 0
		m.status = fmt.Sprintf("Cleared %d queued image(s)", count)
	case "c":
		m.beginManualCapture()
		m.returnToImageManager = true
		commands = append(commands, m.startCameraWindow())
	case "t":
		if !m.cameraAvailable {
			if !m.cfg.AllowPlaintextPreview {
				m.err = errors.New("terminal preview is disabled because ImageSnap briefly writes plaintext frames; restart with --allow-plaintext-terminal-preview to opt in")
			} else {
				m.err = errors.New("terminal camera backend unavailable; install ImageSnap with: brew install imagesnap")
			}
			m.status = m.err.Error()
			break
		}
		m.beginManualCapture()
		m.returnToImageManager = true
		commands = append(commands, m.startCameraPreview())
	case "f":
		m.beginManualCapture()
		m.returnToImageManager = true
		m.phase = phaseFilePicking
		m.status = "Choose an image · q returns to the queue"
		commands = append(commands, m.filePicker.Init())
	}
	return m, tea.Batch(commands...)
}

func (m *appModel) openScheduleManager(now time.Time) {
	m.phase = phaseScheduleManager
	m.composer.Blur()
	m.err = nil
	m.scheduleManagerSelected = make(map[int]struct{})
	m.scheduleActionCursor = 0
	if err := m.reloadScheduleManager(now); err != nil {
		m.err = err
		m.status = "Could not load schedule: " + err.Error()
		return
	}
	if len(m.scheduleManagerEntries) == 0 {
		m.status = "Schedule is empty"
		return
	}
	m.status = "Select items, then exit to choose how to complete them"
}

func (m *appModel) reloadScheduleManager(now time.Time) error {
	if m.schedule == nil {
		m.scheduleManagerEntries = nil
		m.scheduleManagerCursor = 0
		return errors.New("schedule store is unavailable")
	}
	if err := m.schedule.MaterializeRecurrences(now, defaultScheduleUpcomingWindow); err != nil {
		return err
	}
	entries, err := m.schedule.List()
	if err != nil {
		return err
	}
	openEntries := make([]scheduleEntry, 0, len(entries))
	doneEntries := make([]scheduleEntry, 0, len(entries))
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.CompletedAt == nil {
			openEntries = append(openEntries, entry)
		} else {
			doneEntries = append(doneEntries, entry)
		}
	}
	entries = append(openEntries, doneEntries...)
	m.scheduleManagerEntries = entries
	openIDs := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		if entry.CompletedAt == nil {
			openIDs[entry.ID] = struct{}{}
		}
	}
	for id := range m.scheduleManagerSelected {
		if _, open := openIDs[id]; !open {
			delete(m.scheduleManagerSelected, id)
		}
	}
	if len(entries) == 0 {
		m.scheduleManagerCursor = 0
		return nil
	}
	if m.scheduleManagerCursor >= len(entries) {
		m.scheduleManagerCursor = len(entries) - 1
	}
	if entries[m.scheduleManagerCursor].CompletedAt != nil {
		for index, entry := range entries {
			if entry.CompletedAt == nil {
				m.scheduleManagerCursor = index
				break
			}
		}
	}
	return nil
}

func (m appModel) updateScheduleManager(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, tea.Batch(commands...)
	}
	switch key.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+s", "esc", "q":
		if len(m.scheduleManagerSelected) == 0 {
			m.phase = phaseChatting
			m.status = "Ready"
			commands = append(commands, m.composer.Focus())
		} else {
			m.phase = phaseScheduleAction
			m.scheduleActionCursor = 0
			m.scheduleComment.Reset()
			m.err = nil
			m.status = "Choose how to handle the selected items and add a required comment"
			commands = append(commands, m.scheduleComment.Focus())
		}
	case "up", "k":
		if m.scheduleManagerCursor > 0 {
			m.scheduleManagerCursor--
		}
	case "down", "j":
		if m.scheduleManagerCursor+1 < len(m.scheduleManagerEntries) {
			m.scheduleManagerCursor++
		}
	case "r":
		if err := m.reloadScheduleManager(time.Now()); err != nil {
			m.err = err
			m.status = "Could not refresh schedule: " + err.Error()
		} else {
			m.err = nil
			m.status = "Schedule refreshed"
		}
	case " ", "enter", "c":
		if len(m.scheduleManagerEntries) == 0 {
			m.status = "Schedule is empty"
			break
		}
		entry := m.scheduleManagerEntries[m.scheduleManagerCursor]
		if entry.CompletedAt != nil {
			m.status = fmt.Sprintf("Scheduled item #%d is already complete and cannot be selected", entry.ID)
			break
		}
		if m.scheduleManagerSelected == nil {
			m.scheduleManagerSelected = make(map[int]struct{})
		}
		if _, selected := m.scheduleManagerSelected[entry.ID]; selected {
			delete(m.scheduleManagerSelected, entry.ID)
		} else {
			m.scheduleManagerSelected[entry.ID] = struct{}{}
		}
		m.status = fmt.Sprintf("%d item(s) selected", len(m.scheduleManagerSelected))
	}
	return m, tea.Batch(commands...)
}

func (m appModel) selectedScheduleEntries() []scheduleEntry {
	entries := make([]scheduleEntry, 0, len(m.scheduleManagerSelected))
	for _, entry := range m.scheduleManagerEntries {
		if _, selected := m.scheduleManagerSelected[entry.ID]; selected {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (m appModel) updateScheduleAction(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "ctrl+s":
			m.scheduleComment.Blur()
			m.phase = phaseScheduleManager
			m.err = nil
			m.status = fmt.Sprintf("%d item(s) selected", len(m.scheduleManagerSelected))
			return m, tea.Batch(commands...)
		case "up", "shift+tab":
			m.scheduleActionCursor = max(0, m.scheduleActionCursor-1)
			return m, tea.Batch(commands...)
		case "down", "tab":
			m.scheduleActionCursor = min(1, m.scheduleActionCursor+1)
			return m, tea.Batch(commands...)
		case "shift+enter", "ctrl+j":
			m.scheduleComment.InsertString("\n")
			return m, tea.Batch(commands...)
		case "enter":
			return m.applyScheduleAction(commands)
		}
	}

	var cmd tea.Cmd
	m.scheduleComment, cmd = m.scheduleComment.Update(msg)
	commands = append(commands, cmd)
	return m, tea.Batch(commands...)
}

func (m appModel) applyScheduleAction(commands []tea.Cmd) (tea.Model, tea.Cmd) {
	selected := m.selectedScheduleEntries()
	if len(selected) == 0 {
		m.phase = phaseChatting
		m.status = "Ready"
		commands = append(commands, m.composer.Focus())
		return m, tea.Batch(commands...)
	}

	comment := strings.TrimSpace(m.scheduleComment.Value())
	if comment == "" {
		m.err = errors.New("a completion comment is required")
		m.status = m.err.Error()
		return m, tea.Batch(commands...)
	}

	if m.scheduleActionCursor == 1 {
		for _, entry := range selected {
			m.pendingScheduleRequests = append(m.pendingScheduleRequests, scheduleCompletionRequest{
				Entry:   entry,
				Comment: comment,
			})
		}
		m.scheduleManagerSelected = nil
		m.phase = phaseChatting
		m.scheduleComment.Blur()
		m.err = nil
		m.status = fmt.Sprintf("Queued %d completion request(s) for the next agent turn", len(selected))
		commands = append(commands, m.composer.Focus())
		return m, tea.Batch(commands...)
	}

	var completionErrors []error
	completedCount := 0
	var completionChips []transcriptChip
	if m.pendingScheduleCompletions == nil {
		m.pendingScheduleCompletions = make(map[int]scheduleEntry)
	}
	completedBy := strings.TrimSpace(m.userLabel)
	if completedBy == "" {
		completedBy = "USER"
	}
	for _, entry := range selected {
		completed, err := m.schedule.Complete(entry.ID, comment, completedBy, time.Now())
		if err != nil {
			completionErrors = append(completionErrors, err)
			continue
		}
		completedCount++
		m.pendingScheduleCompletions[completed.ID] = completed
		completionChips = append(completionChips, transcriptChip{
			Kind:  "schedule",
			Label: "Schedule Complete",
		})
		delete(m.scheduleManagerSelected, completed.ID)
	}
	if len(completionChips) > 0 {
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "notice",
			Chips:      completionChips,
			OccurredAt: time.Now(),
		})
		m.refreshTranscript()
		m.viewport.GotoBottom()
	}
	if err := errors.Join(completionErrors...); err != nil {
		m.err = err
		m.status = fmt.Sprintf("Marked %d complete; some items failed: %v", completedCount, err)
		if reloadErr := m.reloadScheduleManager(time.Now()); reloadErr != nil {
			m.status += " · refresh failed: " + reloadErr.Error()
		}
		return m, tea.Batch(commands...)
	}

	m.scheduleManagerSelected = nil
	m.phase = phaseChatting
	m.err = nil
	m.scheduleComment.Blur()
	m.status = fmt.Sprintf("Marked %d scheduled item(s) complete · agent update queued", completedCount)
	commands = append(commands, m.composer.Focus())
	return m, tea.Batch(commands...)
}

func (m *appModel) removeQueuedImage(index int) error {
	if index < 0 || index >= len(m.pendingImages) {
		return errors.New("queued image selection is out of range")
	}
	path := m.pendingImages[index]
	if err := m.consumeOwnedImages([]string{path}); err != nil {
		return err
	}
	copy(m.pendingImages[index:], m.pendingImages[index+1:])
	m.pendingImages[len(m.pendingImages)-1] = ""
	m.pendingImages = m.pendingImages[:len(m.pendingImages)-1]
	if len(m.pendingImages) == 0 {
		m.imageManagerCursor = 0
	} else if m.imageManagerCursor >= len(m.pendingImages) {
		m.imageManagerCursor = len(m.pendingImages) - 1
	}
	return nil
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
		returnToManager := m.returnToImageManager
		m.resetCaptureState()
		m.phase = phaseChatting
		if returnToManager {
			m.phase = phaseImageManager
		}
		if declined || path == "" {
			m.status = "Photo capture cancelled"
		} else {
			m.pendingImages = append(m.pendingImages, path)
			queued := queuedImagesSummary(len(m.pendingImages))
			m.status = fmt.Sprintf("%s for your next message · latest: %s", queued, filepath.Base(path))
			m.transcript = append(m.transcript, transcriptMessage{
				Role:       "system",
				Content:    fmt.Sprintf("A camera photo was captured and queued. %s total; none have been sent yet.", queued),
				Images:     []string{filepath.Base(path)},
				OccurredAt: time.Now(),
			})
			m.refreshTranscript()
			m.viewport.GotoBottom()
		}
		if returnToManager {
			return nil
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
	m.returnToImageManager = false
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
	metadataJSON := m.buildResponseMetadata(now)
	m.lastSent = now
	_ = m.memory.RecordUserActivity(now)

	var content any = value
	if len(images) > 0 {
		parts, err := imagePartsWithVault(value, images, m.captureVault)
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
	m.messages = append(
		m.messages,
		apiMessage{Role: "system", Content: responseMetadataSystemPrompt(metadataJSON)},
		apiMessage{Role: "user", Content: content},
	)
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
			Content:    "Commands: /compact · /settings · /tools [enable NAME | disable NAME] · /memory [list [CATEGORY] | categories | search QUERY] · /whoop · /schedule · /camera · /image PATH… · /images · /clear-images · /debug [on|off] · /model · /system TEXT · /clear · /help\nKeys: Enter send · Shift+Enter newline (Ctrl+J fallback) · Ctrl+S schedule · Ctrl+I image queue · Ctrl+O compact transcript · Ctrl+G models · Ctrl+D debug · PgUp/PgDn scroll · Ctrl+C quit",
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
	case "/compact":
		return true, m.beginMidSessionCompaction()
	case "/settings", "/preferences":
		if m.busy {
			m.status = "Wait for the current reply before opening settings"
			return true, nil
		}
		m.openSettings()
		return true, nil
	case "/tools", "/capabilities":
		if m.client == nil {
			m.status = "Choose a model first"
			return true, nil
		}
		subcommand, rest, _ := strings.Cut(strings.TrimSpace(argument), " ")
		rest = strings.TrimSpace(rest)
		switch strings.ToLower(subcommand) {
		case "":
		case "enable", "on":
			capability, already, err := m.client.EnableCapability(rest)
			if err != nil {
				m.status = err.Error()
				return true, nil
			}
			m.status = "Enabled " + capability.Name + " tools"
			if already {
				m.status = capability.Name + " tools were already enabled"
			}
		case "disable", "off":
			capability, err := m.client.DisableCapability(rest)
			if err != nil {
				m.status = err.Error()
				return true, nil
			}
			m.status = "Disabled " + capability.Name + " tools"
		default:
			m.status = "Usage: /tools [enable NAME | disable NAME]"
			return true, nil
		}
		m.transcript = append(m.transcript, transcriptMessage{Role: "notice", Content: m.toolsNotice(), OccurredAt: time.Now(), Meta: "tools"})
	case "/memory", "/memories":
		subcommand, rest, _ := strings.Cut(strings.TrimSpace(argument), " ")
		rest = strings.TrimSpace(rest)
		var content string
		switch strings.ToLower(subcommand) {
		case "", "list", "ls":
			content = m.memory.List(rest)
		case "categories", "cats":
			content = m.memoryCategoriesNotice()
		case "search", "find":
			if rest == "" {
				m.status = "Usage: /memory search QUERY"
				return true, nil
			}
			content = m.memory.Search(rest, "")
		default:
			m.status = "Usage: /memory [list [CATEGORY] | categories | search QUERY]"
			return true, nil
		}
		m.transcript = append(m.transcript, transcriptMessage{Role: "notice", Content: content, OccurredAt: time.Now(), Meta: "memory"})
	case "/model":
		m.phase = phasePicking
		m.composer.Blur()
		m.status = "Choose a model · the current transcript will reset"
	case "/clear":
		m.transcript = nil
	case "/images":
		m.openImageManager()
	case "/schedule", "/schedules":
		m.openScheduleManager(time.Now())
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
		m.status = queuedImagesSummary(len(m.pendingImages)) + " for the next message"
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

func (m *appModel) toggleCompactTranscript() {
	m.compactTranscript = !m.compactTranscript
	m.status = "Compact transcript disabled"
	if m.compactTranscript {
		m.status = "Compact transcript enabled"
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
This is a private control-plane timer event, not a user message or a new conversation stage. The preceding conversation is unchanged.

Before finishing, do the follow:
1. Reconcile the 'schedule' with recent chat messages:
  - read both assistant and user messages to see if any task, goal, assignment, etc has been scheduled
  - check if there is a corresponding event in the schedule log, using 'list_scheduled_items' toolcalls if necessary.
  - if something is missing from the schedule, add it with a 'schedule_item' tool call.
  - if the user claimed to do something and it matches a recent upcoming item, mark it as done.
2. Reconcile memories
  - scan recent messages for unique details about the user, goals, etc.
  - check if the information is encoded an existing memory
  - if it isn't, add it.

Perform any maintenance tool calls in earlier rounds. Then call complete_heartbeat exactly once and alone. Use action "none" when no separate user-facing message is warranted, including after silent maintenance. Use action "notify" only with a complete message genuinely worth surfacing. Do not answer in prose.

Visibility rule: no response has been sent merely because you drafted one. Your reasoning_content, thinking text, drafts, and ordinary assistant prose are private and discarded during a heartbeat. The only way to show text to the user is complete_heartbeat with action "notify" and the entire final response in message. If you drafted or intend any user-facing response, copy it into message and choose "notify". Choosing "none" asserts that the user should see nothing.

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
	m.scheduleComment.SetWidth(max(20, min(bodyWidth-8, 78)))
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
	case phaseImageManager:
		content = m.renderImageManager()
	case phaseScheduleManager:
		content = m.renderScheduleManager()
	case phaseScheduleAction:
		content = m.renderScheduleAction()
	case phaseCompacting:
		content = m.renderCompacting()
	case phaseSettings:
		content = m.renderSettings()
	case phaseCategoryEditor:
		content = m.renderCategoryEditor()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	// Cell motion enables click, release, and wheel events without reporting
	// every pointer movement. This lets trackpads and mouse wheels scroll the
	// transcript; terminals may require a modifier for text selection.
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
	help := lipgloss.NewStyle().Foreground(m.palette.muted).Render("↑/↓ move  ·  enter connect  ·  r refresh  ·  F2 settings  ·  q quit")
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
	footerText := "Enter send  ·  Shift+Enter newline  ·  F2 settings  ·  Ctrl+S schedule  ·  Ctrl+I images  ·  Ctrl+O compact  ·  Ctrl+G models  ·  Ctrl+D debug  ·  Ctrl+C quit"
	if m.busy {
		footerText = "Agent working · keep drafting · wait for Ready, then Enter to send · PgUp/PgDn scroll · Ctrl+C quit"
	}
	if len(m.pendingImages) > 0 {
		footerText = queuedImagesSummary(len(m.pendingImages)) + " · Enter sends all · Ctrl+I manage · Ctrl+S schedule · Ctrl+O compact"
		if m.busy {
			footerText = queuedImagesSummary(len(m.pendingImages)) + " · agent working · draft now, send when Ready"
		}
	}
	footer := lipgloss.NewStyle().
		Foreground(m.palette.muted).
		Render(footerText)
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
	queueNotice := ""
	if m.captureManual && len(m.pendingImages) > 0 {
		queueNotice = fmt.Sprintf(
			"\n\n%s already; approving another photo will make %d.",
			queuedImagesSummary(len(m.pendingImages)),
			len(m.pendingImages)+1,
		)
	}
	content := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.accent).
		Padding(1, 2).
		Width(max(40, min(m.width-8, 92))).
		Render(
			lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render(title) +
				"\n\n" + body + "\n\n" + why + "\n\n" + privacy +
				lipgloss.NewStyle().Foreground(m.palette.success).Render(queueNotice) +
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

func (m appModel) renderImageManager() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Image queue")
	summary := lipgloss.NewStyle().Foreground(m.palette.accent).Render(
		queuedImagesSummary(len(m.pendingImages)) + " for your next message",
	)
	availableRows := max(3, m.height-14)
	start := 0
	if m.imageManagerCursor >= availableRows {
		start = m.imageManagerCursor - availableRows + 1
	}
	end := min(len(m.pendingImages), start+availableRows)
	rows := make([]string, 0, max(1, end-start))
	if len(m.pendingImages) == 0 {
		rows = append(rows, lipgloss.NewStyle().Foreground(m.palette.muted).Render(
			"No images are queued. Add one with c, t, or f.",
		))
	} else {
		for index := start; index < end; index++ {
			path := m.pendingImages[index]
			name := filepath.Base(path)
			source := "file"
			if m.captureVault != nil && m.captureVault.Owns(path) {
				source = "camera · encrypted until sent"
				name = strings.TrimSuffix(name, ".jpg.enc")
			}
			detail := source + " · " + path
			cursor := "  "
			rowStyle := lipgloss.NewStyle().Foreground(m.palette.text)
			if index == m.imageManagerCursor {
				cursor = "› "
				rowStyle = rowStyle.Bold(true).Foreground(m.palette.secondary)
			}
			rows = append(rows, rowStyle.Render(fmt.Sprintf("%s%d. %s\n    %s", cursor, index+1, name, detail)))
		}
	}
	controls := lipgloss.NewStyle().Foreground(m.palette.secondary).Render(
		"↑/↓ select · d/x remove · Ctrl+X clear all · c camera · t terminal camera · f file · Ctrl+I/esc done",
	)
	status := lipgloss.NewStyle().Foreground(m.palette.muted).Render(m.status)
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.primary).
		Padding(1, 2).
		Width(max(44, min(m.width-8, 100))).
		Render(title + "\n" + summary + "\n\n" + strings.Join(rows, "\n\n") + "\n\n" + controls + "\n\n" + status)
	return lipgloss.NewStyle().Padding(1, 3).Render(panel)
}

func (m appModel) renderScheduleManager() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Schedule")
	openCount := 0
	completedCount := 0
	for _, entry := range m.scheduleManagerEntries {
		if entry.CompletedAt == nil {
			openCount++
		} else {
			completedCount++
		}
	}
	summaryText := fmt.Sprintf("%d open · %d complete", openCount, completedCount)
	summary := lipgloss.NewStyle().Foreground(m.palette.accent).Render(summaryText)

	panelWidth := max(44, min(m.width-8, 100))
	contentWidth := max(20, panelWidth-6)
	// Entries render as a header line plus the event text wrapped beneath it,
	// so paging works in lines rather than entries: the window always
	// contains the cursor entry and as many neighbours as fit.
	availableLines := max(3, m.height-14)
	var rows []string
	if len(m.scheduleManagerEntries) == 0 {
		rows = append(rows, lipgloss.NewStyle().Foreground(m.palette.muted).Render(
			"No scheduled items. The agent can add them with its scheduling tool.",
		))
	} else {
		now := time.Now()
		sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(m.palette.accent)
		sectionOf := func(entry scheduleEntry) string {
			if entry.CompletedAt != nil {
				return "DONE"
			}
			return "UPCOMING & DUE"
		}
		blocks := make([][]string, len(m.scheduleManagerEntries))
		for index, entry := range m.scheduleManagerEntries {
			blocks[index] = m.renderScheduleEntry(entry, index == m.scheduleManagerCursor, now, contentWidth)
		}
		// Lines each entry costs inside the window, counting a section heading
		// whenever the section differs from the previous entry's.
		cost := func(index int) int {
			lines := len(blocks[index])
			if index == 0 || sectionOf(m.scheduleManagerEntries[index]) != sectionOf(m.scheduleManagerEntries[index-1]) {
				lines++
			}
			return lines
		}
		cursor := min(m.scheduleManagerCursor, len(blocks)-1)
		start, used := cursor, cost(cursor)
		for start > 0 && used+cost(start-1) <= availableLines {
			start--
			used += cost(start)
		}
		end := cursor + 1
		for end < len(blocks) && used+cost(end) <= availableLines {
			used += cost(end)
			end++
		}
		currentSection := ""
		for index := start; index < end; index++ {
			if section := sectionOf(m.scheduleManagerEntries[index]); section != currentSection {
				rows = append(rows, sectionStyle.Render(section))
				currentSection = section
			}
			rows = append(rows, blocks[index]...)
		}
		if start > 0 || end < len(blocks) {
			rows = append(rows, lipgloss.NewStyle().Foreground(m.palette.muted).Render(
				fmt.Sprintf("showing %d–%d of %d", start+1, end, len(blocks)),
			))
		}
	}
	controls := lipgloss.NewStyle().Foreground(m.palette.secondary).Render(
		ansi.Truncate("↑/↓ move · Enter/Space select · r refresh · Ctrl+S/esc done", contentWidth, "…"),
	)
	status := lipgloss.NewStyle().Foreground(m.palette.muted).Render(ansi.Truncate(m.status, contentWidth, "…"))
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.primary).
		Padding(1, 2).
		Width(panelWidth).
		Render(title + "\n" + summary + "\n\n" + strings.Join(rows, "\n") + "\n\n" + controls + "\n" + status)
	return lipgloss.NewStyle().Padding(1, 3).Render(panel)
}

func (m appModel) renderScheduleAction() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Finish schedule changes?")
	count := len(m.scheduleManagerSelected)
	body := fmt.Sprintf("%d selected item(s) can be completed here or sent to the agent as a request. The update accompanies the next normal message or heartbeat.", count)
	options := []string{
		"Mark the selected items done now",
		"Request that the agent mark them done",
	}
	rows := make([]string, 0, len(options))
	for index, option := range options {
		cursor := "  "
		style := lipgloss.NewStyle().Foreground(m.palette.text)
		if index == m.scheduleActionCursor {
			cursor = "› "
			style = style.Bold(true).Foreground(m.palette.secondary).Background(m.palette.surface)
		}
		rows = append(rows, style.Render(cursor+option))
	}
	commentLabel := lipgloss.NewStyle().Bold(true).Foreground(m.palette.accent).Render("Required comment")
	controls := lipgloss.NewStyle().Foreground(m.palette.muted).Render(
		"↑/↓ choose · Enter submit · Shift+Enter newline · esc back",
	)
	panelWidth := max(44, min(m.width-8, 88))
	contentWidth := max(20, panelWidth-6)
	statusLine := ""
	if m.err != nil {
		statusLine = "\n" + lipgloss.NewStyle().Foreground(m.palette.danger).Render(
			ansi.Truncate(m.status, contentWidth, "…"),
		)
	}
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.accent).
		Padding(1, 2).
		Width(panelWidth).
		Render(
			title + "\n\n" +
				ansi.Truncate(body, contentWidth, "…") + "\n\n" +
				strings.Join(rows, "\n") + "\n\n" +
				commentLabel + "\n" +
				m.scheduleComment.View() + "\n" +
				ansi.Truncate(controls, contentWidth, "…") +
				statusLine,
		)
	return lipgloss.NewStyle().Padding(2, 4).Render(panel)
}

func (m appModel) renderPhotoReview() string {
	title := "Review captured photo"
	if !m.captureManual {
		title = "Review before sharing with the model"
	} else if len(m.pendingImages) > 0 {
		title += fmt.Sprintf(" · %s already", queuedImagesSummary(len(m.pendingImages)))
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

func queuedImagesSummary(count int) string {
	if count == 1 {
		return "1 image queued"
	}
	return fmt.Sprintf("%d images queued", count)
}

func mutationNoticeChips(notices []string) ([]transcriptChip, []string) {
	chips := make([]transcriptChip, 0, len(notices))
	remaining := make([]string, 0, len(notices))
	for _, notice := range notices {
		switch {
		case notice == "memory saved":
			chips = append(chips, transcriptChip{Kind: "memory", Label: "Memory Saved"})
		case notice == "memory updated":
			chips = append(chips, transcriptChip{Kind: "memory", Label: "Memory Updated"})
		case strings.HasPrefix(notice, "enabled capability "):
			chips = append(chips, transcriptChip{Kind: "tools", Label: "Tools: " + strings.TrimPrefix(notice, "enabled capability ")})
		case strings.HasPrefix(notice, "removed "):
			var count int
			if _, err := fmt.Sscanf(notice, "removed %d memory entry(s)", &count); err == nil && count > 0 {
				chips = append(chips, transcriptChip{
					Kind:  "memory",
					Label: "Memory Removed",
				})
			} else {
				remaining = append(remaining, notice)
			}
		case strings.HasPrefix(notice, "scheduled recurring series #"):
			chips = append(chips, transcriptChip{Kind: "schedule", Label: "Schedule Created"})
		case strings.HasPrefix(notice, "scheduled item #"):
			chips = append(chips, transcriptChip{Kind: "schedule", Label: "Schedule Created"})
		case strings.HasPrefix(notice, "completed scheduled item #"):
			chips = append(chips, transcriptChip{Kind: "schedule", Label: "Schedule Complete"})
		default:
			remaining = append(remaining, notice)
		}
	}
	return chips, remaining
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
	if m.compactTranscript {
		right += "  ·  COMPACT"
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
		case "notice":
			color = m.palette.success
			label = "UPDATED"
		case "error":
			color = m.palette.danger
		}
		if m.compactTranscript {
			switch message.Role {
			case "user", "assistant", "assistant_stream":
				label = firstCharacter(label)
			case "reasoning":
				label = firstCharacter(label) + "·T"
			}
			body := compactMarkdownText(message.Content, width)
			if len(message.Images) > 0 {
				body += " [images: " + strings.Join(message.Images, ", ") + "]"
			}
			if len(message.Chips) > 0 {
				chips := compactTranscriptChips(message.Chips)
				if body != "" {
					body += " "
				}
				body += chips
			}
			blocks = append(blocks, compactTranscriptLine(label, body, width))
			continue
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
		if len(message.Chips) > 0 {
			if body != "" {
				body += "\n"
			}
			body += m.renderTranscriptChips(message.Chips, max(12, width-4))
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
	separator := "\n\n"
	if m.compactTranscript {
		separator = "\n"
	}
	return strings.Join(blocks, separator)
}

func (m appModel) renderTranscriptChips(chips []transcriptChip, width int) string {
	rows := make([]string, 0, len(chips))
	current := make([]string, 0, len(chips))
	currentWidth := 0
	flush := func() {
		if len(current) == 0 {
			return
		}
		rows = append(rows, strings.Join(current, " "))
		current = nil
		currentWidth = 0
	}
	for _, chip := range chips {
		background := m.palette.secondary
		symbol := "◆"
		if chip.Kind == "memory" {
			background = m.palette.success
			symbol = "✦"
		}
		label := ansi.Truncate(symbol+" "+chip.Label, max(1, width-4), "…")
		leftCap := lipgloss.NewStyle().
			Foreground(background).
			Render("")
		middle := lipgloss.NewStyle().
			Bold(true).
			Foreground(m.palette.surface).
			Background(background).
			Render(" " + label + " ")
		rightCap := lipgloss.NewStyle().
			Foreground(background).
			Render("")
		rendered := leftCap + middle + rightCap
		renderedWidth := lipgloss.Width(rendered)
		gap := 0
		if len(current) > 0 {
			gap = 1
		}
		if currentWidth+gap+renderedWidth > width {
			flush()
		}
		current = append(current, rendered)
		currentWidth += renderedWidth
		if len(current) > 1 {
			currentWidth++
		}
	}
	flush()
	return strings.Join(rows, "\n")
}

func compactTranscriptChips(chips []transcriptChip) string {
	rendered := make([]string, 0, len(chips))
	for _, chip := range chips {
		rendered = append(rendered, "["+chip.Label+"]")
	}
	return strings.Join(rendered, " ")
}

func compactTranscriptLine(label, body string, width int) string {
	prefix := label + ":"
	if body == "" {
		return prefix
	}
	prefix += " "
	prefixWidth := ansi.StringWidth(prefix)
	if width <= prefixWidth {
		return ansi.Wrap(prefix+body, width, "")
	}

	lines := strings.Split(ansi.Wrap(body, width-prefixWidth, ""), "\n")
	indent := strings.Repeat(" ", prefixWidth)
	for index := range lines {
		if index == 0 {
			lines[index] = prefix + lines[index]
		} else {
			lines[index] = indent + lines[index]
		}
	}
	return strings.Join(lines, "\n")
}

func firstCharacter(value string) string {
	for _, character := range value {
		return string(character)
	}
	return "A"
}

func compactMarkdownText(content string, width int) string {
	rendered := ansi.Strip(renderMarkdown(content, width))
	lines := strings.Split(rendered, "\n")
	plain := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = stripCompactLinePrefix(line)
		if line != "" {
			plain = append(plain, line)
		}
	}
	return strings.Join(strings.Fields(strings.Join(plain, " ")), " ")
}

func stripCompactLinePrefix(line string) string {
	if strings.HasPrefix(line, "#") {
		index := 0
		for index < len(line) && line[index] == '#' {
			index++
		}
		if index < len(line) && line[index] == ' ' {
			line = strings.TrimSpace(line[index:])
		}
	}
	for _, prefix := range []string{"│", ">", "•", "- ", "* ", "+ "} {
		if strings.HasPrefix(line, prefix) {
			line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
			break
		}
	}
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index > 0 && index+1 < len(line) && line[index] == '.' && line[index+1] == ' ' {
		line = strings.TrimSpace(line[index+2:])
	}
	if strings.Trim(line, "-*_") == "" {
		return ""
	}
	return line
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
	UpcomingThrough    string             `json:"upcoming_through"`
	Due                []scheduleItemView `json:"due"`
	Upcoming           []scheduleItemView `json:"upcoming"`
	NextLater          *scheduleItemView  `json:"next_later"`
	RecentlyCompleted  []scheduleItemView `json:"recently_completed,omitempty"`
	CompletionRequests []string           `json:"completion_requests,omitempty"`
}

func (m *appModel) responseEnvelope(text string, now time.Time) string {
	enveloped, _ := m.buildResponseEnvelope(text, now)
	return enveloped
}

func (m *appModel) buildResponseEnvelope(text string, now time.Time) (string, string) {
	metadataJSON := m.buildResponseMetadata(now)
	return "[HARNESS METADATA]\n" + metadataJSON + "\n[/HARNESS METADATA]\n" + text, metadataJSON
}

func responseMetadataSystemPrompt(metadataJSON string) string {
	return `This is private harness context attached to the next user turn. It is not a request, not user-authored text, and not part of the conversation. Use relevant fields silently when responding to the adjacent user message. Never quote, summarize, mention, or expose this context.

` + metadataJSON
}

func (m *appModel) buildResponseMetadata(now time.Time) string {
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
		}

		unseenCompleted := make([]scheduleEntry, 0, len(snapshot.RecentCompleted)+len(m.pendingScheduleCompletions))
		includedCompletions := make(map[int]struct{}, cap(unseenCompleted))
		if err == nil {
			for _, entry := range snapshot.RecentCompleted {
				if _, seen := m.seenCompletions[entry.ID]; !seen {
					unseenCompleted = append(unseenCompleted, entry)
					includedCompletions[entry.ID] = struct{}{}
				}
			}
		}
		for id, entry := range m.pendingScheduleCompletions {
			if _, included := includedCompletions[id]; included {
				continue
			}
			if _, seen := m.seenCompletions[id]; seen {
				continue
			}
			unseenCompleted = append(unseenCompleted, entry)
			includedCompletions[id] = struct{}{}
		}
		sortScheduleEntries(unseenCompleted)

		requestedEntries := append([]scheduleCompletionRequest(nil), m.pendingScheduleRequests...)
		completionRequests := make([]string, 0, len(requestedEntries))
		userLabel := strings.TrimSpace(m.userLabel)
		if userLabel == "" {
			userLabel = "USER"
		}
		for _, request := range requestedEntries {
			completionRequests = append(completionRequests, fmt.Sprintf(
				"%s requests scheduled item #%d (%q) be marked as complete with this comment: %q",
				userLabel,
				request.Entry.ID,
				request.Entry.Event,
				request.Comment,
			))
		}

		if len(snapshot.Due)+len(snapshot.Upcoming)+len(snapshot.Later)+len(unseenCompleted)+len(completionRequests) > 0 {
			later := humanScheduleViews(snapshot.Later, now)
			metadata.Schedule = &responseEnvelopeSchedule{
				UpcomingThrough:    humanTimestamp(now.Add(defaultScheduleUpcomingWindow), now.Location()),
				Due:                humanScheduleViews(snapshot.Due, now),
				Upcoming:           humanScheduleViews(snapshot.Upcoming, now),
				RecentlyCompleted:  humanScheduleViews(unseenCompleted, now),
				CompletionRequests: completionRequests,
			}
			if len(later) > 0 {
				metadata.Schedule.NextLater = &later[0]
			}
			if m.seenCompletions == nil {
				m.seenCompletions = make(map[int]struct{})
			}
			for _, entry := range unseenCompleted {
				m.seenCompletions[entry.ID] = struct{}{}
				delete(m.pendingScheduleCompletions, entry.ID)
			}
			m.pendingScheduleRequests = nil
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		encoded = []byte(fmt.Sprintf(`{"sent_at":%q,"since_last_user_message":%q}`, metadata.SentAt, metadata.SinceLastUserMessage))
	}
	return string(encoded)
}

func humanTimestamp(value time.Time, location *time.Location) string {
	return value.In(location).Format("Monday January 2, 2006 at 3:04 PM MST")
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

// beginExit starts the exit sequence. When the session produced at least one
// reply, the model is asked to compact the conversation for the next session
// first; a second Ctrl+C skips that and quits immediately.
func (m appModel) beginExit() (tea.Model, tea.Cmd) {
	m.stopCompletionStream()
	m.disarmHeartbeat()
	if m.client == nil || m.sessionReplies == 0 {
		return m, tea.Quit
	}
	m.busy = true
	m.err = nil
	m.phase = phaseCompacting
	m.compactMidSession = false
	m.composer.Blur()
	m.compactText = ""
	m.compactLog = nil
	m.status = "Asking the model to review and compact the session…"
	return m, m.startCompaction()
}

// beginMidSessionCompaction runs the compaction exchange without exiting and
// then replaces the conversation history with the summary plus the most
// recent turns, so a long session stops growing toward the context ceiling.
func (m *appModel) beginMidSessionCompaction() tea.Cmd {
	if m.client == nil {
		m.status = "Choose a model first"
		return nil
	}
	if m.busy {
		m.status = "Wait for the current reply before compacting"
		return nil
	}
	if m.sessionReplies == 0 {
		m.status = "Nothing to compact yet"
		return nil
	}
	m.stopCompletionStream()
	m.disarmHeartbeat()
	m.busy = true
	m.err = nil
	m.phase = phaseCompacting
	m.compactMidSession = true
	m.composer.Blur()
	m.compactText = ""
	m.compactLog = nil
	m.status = "Compacting the conversation so far…"
	return m.startCompaction()
}

// finishMidSessionCompaction swaps the compacted history in and returns to
// the chat. The history file is not touched; the exit compaction owns it.
func (m *appModel) finishMidSessionCompaction(result completionResult, err error) tea.Cmd {
	m.stopCompaction()
	m.busy = false
	m.phase = phaseChatting
	m.compactMidSession = false
	if err != nil {
		m.err = err
		m.status = "Compaction failed · history unchanged"
		m.transcript = append(m.transcript, transcriptMessage{Role: "error", Content: "Compaction failed: " + err.Error(), OccurredAt: time.Now()})
	} else {
		before := len(m.messages)
		m.messages = compactedHistory(m.messages, m.startupMessageCount, result.Reply, time.Now())
		m.promptTokens = 0
		chips, remaining := mutationNoticeChips(result.Notices)
		chips = append([]transcriptChip{{Kind: "tools", Label: "Conversation Compacted"}}, chips...)
		m.transcript = append(m.transcript, transcriptMessage{
			Role:       "notice",
			Content:    fmt.Sprintf("Earlier turns were replaced by a summary (%d → %d messages in context). The summary is visible with Ctrl+D.", before, len(m.messages)),
			Chips:      chips,
			OccurredAt: time.Now(),
			Meta:       strings.Join(remaining, " · "),
		})
		m.transcript = append(m.transcript, transcriptMessage{Role: "system", Content: result.Reply, OccurredAt: time.Now(), Meta: "compaction summary"})
		m.status = "Conversation compacted"
	}
	m.refreshTranscript()
	m.viewport.GotoBottom()
	var cmd tea.Cmd
	if m.cfg.Heartbeat > 0 {
		cmd = m.armDefaultHeartbeat(time.Now())
	}
	return tea.Batch(cmd, m.composer.Focus())
}

// compactedHistory keeps the startup context, inserts the summary as a
// system note, and retains the last two user turns with their replies.
func compactedHistory(messages []apiMessage, startupCount int, summary string, at time.Time) []apiMessage {
	if startupCount > len(messages) || startupCount < 0 {
		startupCount = 0
		for index, message := range messages {
			if message.Role == "user" {
				break
			}
			startupCount = index + 1
		}
	}
	startup := append([]apiMessage(nil), messages[:startupCount]...)
	body := messages[startupCount:]
	var userIndexes []int
	for index, message := range body {
		if message.Role == "user" {
			userIndexes = append(userIndexes, index)
		}
	}
	var recent []apiMessage
	if len(userIndexes) > 0 {
		from := userIndexes[max(0, len(userIndexes)-2)]
		recent = body[from:]
	}
	note := apiMessage{
		Role: "system",
		Content: fmt.Sprintf("[HARNESS NOTE] The conversation so far was compacted at %s to free context. Earlier turns were replaced by this summary you wrote; the most recent turns follow unchanged.\n\n%s",
			humanTimestamp(at, at.Location()), strings.TrimSpace(summary)),
	}
	return append(append(startup, note), recent...)
}

func (m *appModel) startCompaction() tea.Cmd {
	m.stopCompaction()
	m.compactSession++
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	m.compactCancel = cancel
	session := m.compactSession
	client := m.client
	messages := discardEmbeddedImages(append([]apiMessage(nil), m.messages...))
	request := compactionRequest{Thinking: m.cfg.CompactionThinking, Instructions: strings.TrimSpace(m.cfg.CompactionInstructions), MidSession: m.compactMidSession}
	if !m.compactMidSession {
		if dropping, err := m.memory.CompactionOverflow(m.cfg.CompactionHistory); err == nil {
			request.Dropping = dropping
		}
	}
	if path := strings.TrimSpace(m.cfg.CompactionInstructionsFile); path != "" {
		if content, err := os.ReadFile(expandHome(path)); err == nil {
			request.Instructions = strings.TrimSpace(request.Instructions + "\n\n" + strings.TrimSpace(string(content)))
		} else {
			m.compactLog = append(m.compactLog, "could not read compaction instructions file: "+err.Error())
		}
	}
	if len(request.Dropping) > 0 {
		m.compactLog = append(m.compactLog, fmt.Sprintf("%d older session summar%s will be archived", len(request.Dropping), pluralSuffix(len(request.Dropping), "y", "ies")))
	}
	return func() tea.Msg {
		events := client.compactStream(ctx, messages, request)
		event, ok := <-events
		return compactStreamMsg{session: session, events: events, event: event, ok: ok}
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func pluralSuffix(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func (m *appModel) stopCompaction() {
	if m.compactCancel != nil {
		m.compactCancel()
		m.compactCancel = nil
	}
	m.compactSession++
	m.compactEvents = nil
}

func waitCompactStreamCmd(session uint64, events <-chan completionStreamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		return compactStreamMsg{session: session, events: events, event: event, ok: ok}
	}
}

func (m *appModel) finishCompaction(result completionResult, err error) {
	m.stopCompaction()
	m.busy = false
	if err != nil {
		m.exitNotice = "Compaction failed, previous summary left unchanged: " + err.Error()
		return
	}
	dropped, err := m.memory.AppendCompactedConversation(result.Reply, time.Now(), m.cfg.CompactionHistory)
	if err != nil {
		m.exitNotice = "Could not save the compacted conversation: " + err.Error()
		return
	}
	notice := "Conversation compacted to " + m.memory.compactedConversationPath()
	var details []string
	if len(dropped) > 0 {
		details = append(details, fmt.Sprintf("%d older summar%s removed", len(dropped), pluralSuffix(len(dropped), "y", "ies")))
	}
	if chips, remaining := mutationNoticeChips(result.Notices); len(chips) > 0 || len(remaining) > 0 {
		for _, chip := range chips {
			details = append(details, strings.ToLower(chip.Label))
		}
		details = append(details, remaining...)
	}
	if len(details) > 0 {
		notice += " (" + strings.Join(details, ", ") + ")"
	}
	m.exitNotice = notice
}

func (m appModel) updateCompacting(msg tea.Msg, commands []tea.Cmd) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc":
			m.stopCompaction()
			m.busy = false
			if m.compactMidSession {
				m.compactMidSession = false
				m.phase = phaseChatting
				m.status = "Compaction cancelled · history unchanged"
				commands = append(commands, m.composer.Focus())
				if m.cfg.Heartbeat > 0 {
					commands = append(commands, m.armDefaultHeartbeat(time.Now()))
				}
				return m, tea.Batch(commands...)
			}
			m.exitNotice = "Compaction skipped; previous summary left unchanged."
			return m, tea.Quit
		}
	}
	return m, tea.Batch(commands...)
}

func (m appModel) startupContextSummary() string {
	summary := fmt.Sprintf("%d memories loaded", m.memoryCount)
	if m.compactedLoaded {
		summary += " · previous session summary loaded"
	}
	return summary
}

func (m appModel) renderCompacting() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.palette.primary).Render("Compacting conversation")
	explanation := "The model is reviewing this session for anything to remember or schedule, archiving summaries that fall out of the history, and writing a handoff summary for next time."
	if m.compactMidSession {
		explanation = "The model is reviewing the conversation so far for anything to remember or schedule and writing a summary that will replace the earlier turns in context. The chat continues afterwards."
	}
	body := lipgloss.NewStyle().Foreground(m.palette.text).Render(explanation)
	progress := m.spinner.View() + " " + m.status
	preview := ""
	if len(m.compactLog) > 0 {
		preview = "\n\n" + lipgloss.NewStyle().
			Foreground(m.palette.muted).
			Width(max(30, min(m.width-16, 84))).
			Render(strings.Join(m.compactLog, "\n"))
	}
	controlText := "Ctrl+C again to skip and exit now"
	if m.compactMidSession {
		controlText = "Ctrl+C or esc to cancel and keep the current history"
	}
	controls := lipgloss.NewStyle().Foreground(m.palette.secondary).Render(controlText)
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette.accent).
		Padding(1, 2).
		Width(max(40, min(m.width-8, 92))).
		Render(title + "\n\n" + body + "\n\n" + progress + preview + "\n\n" + controls)
	return lipgloss.NewStyle().Padding(2, 4).Render(panel)
}

func (m appModel) memoryCategoriesNotice() string {
	categories, err := m.memory.Categories()
	if err != nil {
		return "Could not read memory categories: " + err.Error()
	}
	path := m.memory.categoriesPath()
	if len(categories) == 0 {
		return "No memory categories are configured, so the agent may use any category name.\nCreate " + path + " with one category per line as \"name: description\" to restrict and describe them."
	}
	var result strings.Builder
	result.WriteString("Configured memory categories (" + path + "):")
	for _, category := range categories {
		result.WriteString("\n- " + category.Name)
		if category.Description != "" {
			result.WriteString(": " + category.Description)
		}
	}
	return result.String()
}

func (m appModel) toolsNotice() string {
	var result strings.Builder
	if m.client.exposeAllTools {
		result.WriteString("All tool groups are exposed (the default; start with --lean-tools to gate them).")
	} else {
		result.WriteString("Lean mode: core tools are always exposed; the model exposes a group with request_toolcall, or use /tools enable NAME.")
	}
	for _, capability := range toolCapabilities {
		state := "hidden"
		if m.client.capabilityEnabled(capability.Name) {
			state = "enabled"
		}
		result.WriteString("\n- " + capability.Name + " (" + state + "): " + capability.Description)
	}
	return result.String()
}

// preferredChoice finds the online discovered model matching the preferred
// "backend/model" setting.
func (m appModel) preferredChoice() (modelChoice, bool) {
	for _, choice := range m.choices {
		if choice.Online && modelKey(choice) == m.cfg.PreferredModel {
			return choice, true
		}
	}
	return modelChoice{}, false
}

// renderScheduleEntry renders one schedule row as a header line followed by
// the event text and any completion comment wrapped to the panel width.
func (m appModel) renderScheduleEntry(entry scheduleEntry, selected bool, now time.Time, contentWidth int) []string {
	cursor := "  "
	rowStyle := lipgloss.NewStyle().Foreground(m.palette.text)
	if selected {
		cursor = "› "
		rowStyle = rowStyle.Bold(true).Foreground(m.palette.secondary)
	}
	state := "upcoming"
	marker := "[ ]"
	if entry.CompletedAt != nil {
		state = "done"
		marker = "[✓]"
	} else if !entry.Time.After(now) {
		state = "due"
	}
	if _, chosen := m.scheduleManagerSelected[entry.ID]; chosen {
		marker = "[x]"
	}
	recurrence := ""
	if entry.RecurrenceUnit != "" {
		recurrence = fmt.Sprintf(" · repeats every %d %s", entry.RecurrencePeriod, entry.RecurrenceUnit)
		if entry.RecurrencePeriod != 1 {
			recurrence += "s"
		}
	}
	header := fmt.Sprintf("%s%s #%d · %s · %s%s", cursor, marker, entry.ID, entry.Time.Local().Format("Mon Jan 2, 3:04 PM"), state, recurrence)
	lines := []string{rowStyle.Render(ansi.Truncate(header, contentWidth, "…"))}

	indent := "      "
	bodyWidth := max(10, contentWidth-len(indent))
	bodyStyle := lipgloss.NewStyle().Foreground(m.palette.text)
	if selected {
		bodyStyle = bodyStyle.Foreground(m.palette.secondary)
	}
	for _, line := range strings.Split(ansi.Wrap(strings.TrimSpace(entry.Event), bodyWidth, ""), "\n") {
		lines = append(lines, indent+bodyStyle.Render(line))
	}
	if entry.CompletedAt != nil && entry.CompletionComment != "" {
		completedBy := entry.CompletedBy
		if completedBy == "" {
			completedBy = "unknown"
		}
		noteStyle := lipgloss.NewStyle().Foreground(m.palette.muted)
		for _, line := range strings.Split(ansi.Wrap("↳ "+completedBy+": "+strings.TrimSpace(entry.CompletionComment), bodyWidth, ""), "\n") {
			lines = append(lines, indent+noteStyle.Render(line))
		}
	}
	return lines
}
