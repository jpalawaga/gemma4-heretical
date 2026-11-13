package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	defaultDesignModel = "mlx-community/Qwen3-TTS-12Hz-1.7B-VoiceDesign-8bit"
	defaultPresetModel = "mlx-community/Qwen3-TTS-12Hz-1.7B-CustomVoice-8bit"
)

var qwenVoices = []string{
	"Ryan",
	"Aiden",
	"Vivian",
	"Serena",
	"Uncle_Fu",
	"Dylan",
	"Eric",
	"Ono_Anna",
	"Sohee",
}

type config struct {
	Mode        string
	DesignModel string
	PresetModel string
	Voice       string
	Language    string
	Design      string
	Instruct    string
	Speed       float64
	OutputDir   string
	Backend     string
	Timeout     time.Duration
	DisablePlay bool
}

type backend struct {
	Path   string
	Prefix []string
}

type synthesisResult struct {
	Path     string
	Duration time.Duration
	Output   string
	Err      error
}

type model struct {
	cfg        config
	backend    backend
	composer   textarea.Model
	design     textarea.Model
	direction  textarea.Model
	spinner    spinner.Model
	voices     []string
	voiceIndex int
	focus      int
	speed      float64
	width      int
	height     int
	busy       bool
	status     string
	lastAudio  string
	cancel     context.CancelFunc
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "tts:", err)
		os.Exit(2)
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fmt.Fprintln(os.Stderr, "tts: Qwen3-TTS through MLX-Audio requires an Apple Silicon Mac")
		os.Exit(1)
	}
	found, err := findBackend(cfg.Backend)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tts:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "tts:", err)
		os.Exit(1)
	}

	if _, err := tea.NewProgram(newModel(cfg, found)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tts:", err)
		os.Exit(1)
	}
}

func parseConfig(args []string) (config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, err
	}
	flags := flag.NewFlagSet("tts", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	mode := flags.String("mode", "design", "initial voice mode: design or preset")
	designModel := flags.String("model", defaultDesignModel, "Qwen VoiceDesign model ID or local path")
	presetModel := flags.String("preset-model", defaultPresetModel, "Qwen CustomVoice model ID or local path")
	voice := flags.String("voice", "Ryan", "initial Qwen CustomVoice speaker")
	language := flags.String("language", "English", "spoken language name, or Auto")
	design := flags.String("design", "", "initial designed-voice description")
	instruct := flags.String("instruct", "", "initial performance direction for emotion, delivery, and prosody")
	speed := flags.Float64("speed", 1.0, "initial speech speed from 0.5 to 2.0")
	outputDir := flags.String("output-dir", filepath.Join(home, ".gemma4-heretical", "tts"), "directory for generated WAV files")
	backendPath := flags.String("backend", "", "mlx_audio.tts.generate executable; defaults to the installed command or uvx")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum time for one synthesis")
	disablePlay := flags.Bool("no-play", false, "generate without automatically playing audio")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, errors.New("positional arguments are not supported; enter text in the TUI")
	}
	normalizedMode := strings.ToLower(strings.TrimSpace(*mode))
	if normalizedMode != "design" && normalizedMode != "preset" {
		return config{}, errors.New("--mode must be design or preset")
	}
	if strings.TrimSpace(*designModel) == "" {
		return config{}, errors.New("--model cannot be empty")
	}
	if strings.TrimSpace(*presetModel) == "" {
		return config{}, errors.New("--preset-model cannot be empty")
	}
	if strings.TrimSpace(*voice) == "" {
		return config{}, errors.New("--voice cannot be empty")
	}
	if strings.TrimSpace(*language) == "" {
		return config{}, errors.New("--language cannot be empty")
	}
	if *speed < 0.5 || *speed > 2.0 {
		return config{}, errors.New("--speed must be between 0.5 and 2.0")
	}
	if strings.TrimSpace(*outputDir) == "" {
		return config{}, errors.New("--output-dir cannot be empty")
	}
	if *timeout <= 0 {
		return config{}, errors.New("--timeout must be greater than zero")
	}
	absoluteOutput, err := filepath.Abs(*outputDir)
	if err != nil {
		return config{}, err
	}
	return config{
		Mode:        normalizedMode,
		DesignModel: strings.TrimSpace(*designModel),
		PresetModel: strings.TrimSpace(*presetModel),
		Voice:       strings.TrimSpace(*voice),
		Language:    strings.TrimSpace(*language),
		Design:      strings.TrimSpace(*design),
		Instruct:    strings.TrimSpace(*instruct),
		Speed:       *speed,
		OutputDir:   absoluteOutput,
		Backend:     strings.TrimSpace(*backendPath),
		Timeout:     *timeout,
		DisablePlay: *disablePlay,
	}, nil
}

func findBackend(configured string) (backend, error) {
	if configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return backend{}, fmt.Errorf("TTS backend %q was not found: %w", configured, err)
		}
		return backend{Path: path}, nil
	}
	if path, err := exec.LookPath("uvx"); err == nil {
		return backend{
			Path:   path,
			Prefix: []string{"--from", "mlx-audio", "--with", "misaki[en]", "mlx_audio.tts.generate"},
		}, nil
	}
	if path, err := exec.LookPath("mlx_audio.tts.generate"); err == nil {
		return backend{Path: path}, nil
	}
	return backend{}, errors.New("MLX-Audio is unavailable; install uv (https://docs.astral.sh/uv/) or run `uv tool install mlx-audio`")
}

func newModel(cfg config, found backend) model {
	composer := textarea.New()
	composer.Placeholder = "Type something to speak…  Enter generates · Ctrl+J adds a line"
	composer.ShowLineNumbers = false
	composer.CharLimit = 20_000
	composer.SetHeight(8)
	composer.Focus()

	design := textarea.New()
	design.Placeholder = "Who is speaking? e.g. a weathered older woman with a soft Newfoundland accent"
	design.ShowLineNumbers = false
	design.CharLimit = 2_000
	design.SetHeight(3)
	design.SetValue(cfg.Design)
	design.Blur()

	direction := textarea.New()
	direction.Placeholder = "How should this line be performed? e.g. restrained at first, then increasingly urgent"
	direction.ShowLineNumbers = false
	direction.CharLimit = 2_000
	direction.SetHeight(3)
	direction.SetValue(cfg.Instruct)
	direction.Blur()

	spin := spinner.New()
	spin.Spinner = spinner.Dot

	voices := append([]string(nil), qwenVoices...)
	if !slices.Contains(voices, cfg.Voice) {
		voices = append([]string{cfg.Voice}, voices...)
	}
	return model{
		cfg:        cfg,
		backend:    found,
		composer:   composer,
		design:     design,
		direction:  direction,
		spinner:    spin,
		voices:     voices,
		voiceIndex: slices.Index(voices, cfg.Voice),
		speed:      cfg.Speed,
		status:     "Ready",
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.composer.Focus(), m.spinner.Tick)
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var commands []tea.Cmd
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		fieldWidth := max(30, min(92, msg.Width-10))
		m.composer.SetWidth(fieldWidth)
		m.design.SetWidth(fieldWidth)
		m.direction.SetWidth(fieldWidth)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		commands = append(commands, cmd)
	case synthesisResult:
		m.busy = false
		m.cancel = nil
		if msg.Err != nil {
			m.status = "Synthesis failed: " + msg.Err.Error()
			break
		}
		m.lastAudio = msg.Path
		m.status = fmt.Sprintf("Saved %s · generated in %s", msg.Path, msg.Duration.Round(100*time.Millisecond))
		if !m.cfg.DisablePlay {
			if err := playAudio(msg.Path); err != nil {
				m.status += " · playback unavailable: " + err.Error()
			} else {
				m.status += " · playing"
			}
		}
	}

	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c":
			if m.busy && m.cancel != nil {
				m.cancel()
				m.status = "Cancelling…"
				return m, tea.Batch(commands...)
			}
			return m, tea.Quit
		case "ctrl+j":
			if !m.busy {
				switch m.focus {
				case 0:
					m.composer.InsertString("\n")
				case 1:
					m.design.InsertString("\n")
				case 2:
					m.direction.InsertString("\n")
				}
			}
			return m, tea.Batch(commands...)
		case "tab":
			if !m.busy {
				m.moveFocus(1)
				commands = append(commands, m.focusCurrent())
			}
			return m, tea.Batch(commands...)
		case "shift+tab":
			if !m.busy {
				m.moveFocus(-1)
				commands = append(commands, m.focusCurrent())
			}
			return m, tea.Batch(commands...)
		case "ctrl+t":
			if !m.busy {
				if m.cfg.Mode == "design" {
					m.cfg.Mode = "preset"
				} else {
					m.cfg.Mode = "design"
				}
				m.focus = 0
				commands = append(commands, m.focusCurrent())
				m.status = "Mode: " + m.cfg.Mode
			}
			return m, tea.Batch(commands...)
		case "ctrl+n":
			if !m.busy && m.cfg.Mode == "preset" {
				m.voiceIndex = (m.voiceIndex + 1) % len(m.voices)
				m.status = "Voice: " + m.voice()
			}
			return m, tea.Batch(commands...)
		case "ctrl+p":
			if !m.busy && m.cfg.Mode == "preset" {
				m.voiceIndex = (m.voiceIndex - 1 + len(m.voices)) % len(m.voices)
				m.status = "Voice: " + m.voice()
			}
			return m, tea.Batch(commands...)
		case "ctrl+right":
			if !m.busy {
				m.speed = min(2.0, m.speed+0.05)
				m.status = fmt.Sprintf("Speed: %.2fx", m.speed)
			}
			return m, tea.Batch(commands...)
		case "ctrl+left":
			if !m.busy {
				m.speed = max(0.5, m.speed-0.05)
				m.status = fmt.Sprintf("Speed: %.2fx", m.speed)
			}
			return m, tea.Batch(commands...)
		case "enter":
			if !m.busy {
				text := strings.TrimSpace(m.composer.Value())
				if text == "" {
					m.status = "Enter some text first"
				} else if m.cfg.Mode == "design" && strings.TrimSpace(m.design.Value()) == "" {
					m.status = "Describe the voice first"
				} else {
					var cmd tea.Cmd
					m, cmd = m.startSynthesis(text)
					commands = append(commands, cmd)
				}
			}
			return m, tea.Batch(commands...)
		case "ctrl+r":
			if !m.busy && m.lastAudio != "" {
				if err := playAudio(m.lastAudio); err != nil {
					m.status = "Playback unavailable: " + err.Error()
				} else {
					m.status = "Replaying " + m.lastAudio
				}
			}
			return m, tea.Batch(commands...)
		}
	}

	if !m.busy {
		var cmd tea.Cmd
		if m.focus == 0 {
			m.composer, cmd = m.composer.Update(message)
		} else if m.focus == 1 {
			m.design, cmd = m.design.Update(message)
		} else {
			m.direction, cmd = m.direction.Update(message)
		}
		commands = append(commands, cmd)
	}
	return m, tea.Batch(commands...)
}

func (m model) startSynthesis(text string) (model, tea.Cmd) {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
	m.cancel = cancel
	m.busy = true
	m.status = "Loading Qwen3-TTS and generating expressive speech…"
	cfg := m.cfg
	found := m.backend
	voice := ""
	instruct := strings.TrimSpace(m.direction.Value())
	if cfg.Mode == "design" {
		cfg.DesignModel = strings.TrimSpace(cfg.DesignModel)
		instruct = designInstruction(strings.TrimSpace(m.design.Value()), instruct)
	} else {
		voice = m.voice()
	}
	speed := m.speed
	return m, func() tea.Msg {
		defer cancel()
		started := time.Now()
		path, output, err := synthesize(ctx, cfg, found, text, voice, instruct, speed)
		return synthesisResult{
			Path:     path,
			Duration: time.Since(started),
			Output:   output,
			Err:      err,
		}
	}
}

func (m *model) focusCurrent() tea.Cmd {
	m.composer.Blur()
	m.design.Blur()
	m.direction.Blur()
	if m.focus == 1 && m.cfg.Mode == "design" {
		m.status = "Editing voice design"
		return m.design.Focus()
	}
	if m.focus == 2 {
		m.status = "Editing performance direction"
		return m.direction.Focus()
	}
	m.status = "Editing script"
	return m.composer.Focus()
}

func (m *model) moveFocus(delta int) {
	order := []int{0, 2}
	if m.cfg.Mode == "design" {
		order = []int{0, 1, 2}
	}
	index := slices.Index(order, m.focus)
	if index < 0 {
		index = 0
	}
	m.focus = order[(index+delta+len(order))%len(order)]
}

func designInstruction(design, performance string) string {
	if performance == "" {
		return design
	}
	return "Voice identity: " + design + "\nPerformance for this line: " + performance
}

func (m model) voice() string {
	if m.voiceIndex < 0 || m.voiceIndex >= len(m.voices) {
		return m.cfg.Voice
	}
	return m.voices[m.voiceIndex]
}

func synthesize(ctx context.Context, cfg config, found backend, text, voice, instruct string, speed float64) (string, string, error) {
	prefix := "speech-" + time.Now().Format("20060102-150405.000")
	args := append([]string(nil), found.Prefix...)
	modelName := cfg.DesignModel
	if cfg.Mode == "preset" {
		modelName = cfg.PresetModel
	}
	args = append(args,
		"--model", modelName,
		"--text", text,
		"--lang_code", cfg.Language,
		"--speed", strconv.FormatFloat(speed, 'f', 2, 64),
		"--output_path", cfg.OutputDir,
		"--file_prefix", prefix,
		"--audio_format", "wav",
		"--join_audio",
	)
	if voice != "" {
		args = append(args, "--voice", voice)
	}
	if instruct != "" {
		args = append(args, "--instruct", instruct)
	}
	command := exec.CommandContext(ctx, found.Path, args...)
	command.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		message := tail(output.String(), 3000)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", message, fmt.Errorf("timed out after %s", cfg.Timeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", message, errors.New("cancelled")
		}
		if message == "" {
			return "", "", err
		}
		return "", message, fmt.Errorf("%w: %s", err, message)
	}
	path, err := newestMatchingWAV(cfg.OutputDir, prefix)
	if err != nil {
		return "", output.String(), err
	}
	return path, output.String(), nil
}

func newestMatchingWAV(directory, prefix string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(directory, prefix+"*.wav"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("MLX-Audio completed but produced no %s*.wav file in %s", prefix, directory)
	}
	newest := matches[0]
	newestInfo, err := os.Stat(newest)
	if err != nil {
		return "", err
	}
	for _, candidate := range matches[1:] {
		info, statErr := os.Stat(candidate)
		if statErr == nil && info.ModTime().After(newestInfo.ModTime()) {
			newest = candidate
			newestInfo = info
		}
	}
	return newest, nil
}

func playAudio(path string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"afplay", path}}
	default:
		candidates = [][]string{{"paplay", path}, {"aplay", path}, {"ffplay", "-nodisp", "-autoexit", path}}
	}
	for _, candidate := range candidates {
		executable, err := exec.LookPath(candidate[0])
		if err != nil {
			continue
		}
		command := exec.Command(executable, candidate[1:]...)
		if err := command.Start(); err != nil {
			continue
		}
		go func() { _ = command.Wait() }()
		return nil
	}
	return errors.New("no supported audio player was found")
}

func tail(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return "…" + value[len(value)-limit:]
}

func (m model) View() tea.View {
	primary := lipgloss.Color("#A78BFA")
	secondary := lipgloss.Color("#67E8F9")
	muted := lipgloss.Color("#7C8396")
	textColor := lipgloss.Color("#E8EAF2")
	surface := lipgloss.Color("#202333")

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(primary).
		Render("Qwen Expressive TTS")
	subtitle := lipgloss.NewStyle().
		Foreground(muted).
		Render("Local speech synthesis · MLX-Audio · " + m.activeModel())
	settings := lipgloss.NewStyle().
		Foreground(secondary).
		Render(m.settingsLine())
	status := m.status
	if m.busy {
		status = m.spinner.View() + " " + status
	}
	statusLine := lipgloss.NewStyle().Foreground(textColor).Render(status)
	help := lipgloss.NewStyle().
		Foreground(muted).
		Render("Ctrl+T mode · Tab field · Enter speak · Ctrl+J newline · Ctrl+N/P preset voice · Ctrl+←/→ speed · Ctrl+R replay · Ctrl+C quit")

	panelWidth := max(42, min(100, m.width-8))
	content := title + "\n" + subtitle + "\n\n" + settings + "\n\n" +
		lipgloss.NewStyle().Foreground(muted).Render("SCRIPT") + "\n" + m.composer.View() + "\n\n"
	if m.cfg.Mode == "design" {
		content += lipgloss.NewStyle().Foreground(muted).Render("VOICE DESIGN") + "\n" + m.design.View() + "\n\n"
	}
	content += lipgloss.NewStyle().Foreground(muted).Render("PERFORMANCE DIRECTION (optional)") + "\n" + m.direction.View() +
		"\n\n" + statusLine + "\n\n" + help
	panel := lipgloss.NewStyle().
		Width(panelWidth).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primary).
		Background(surface).
		Padding(1, 2).
		Render(content)
	view := tea.NewView(lipgloss.NewStyle().Padding(1, 3).Render(panel))
	view.AltScreen = true
	return view
}

func (m model) activeModel() string {
	if m.cfg.Mode == "preset" {
		return m.cfg.PresetModel
	}
	return m.cfg.DesignModel
}

func (m model) settingsLine() string {
	identity := "designed voice"
	if m.cfg.Mode == "preset" {
		identity = m.voice()
	}
	return fmt.Sprintf("mode  %s    voice  %s    language  %s    speed  %.2fx\noutput  %s",
		m.cfg.Mode, identity, m.cfg.Language, m.speed, m.cfg.OutputDir)
}
