package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// settings is the persistent configuration kept in settings.json. Durations
// are stored as strings such as "15m" so the file stays hand-editable.
// Precedence at startup is defaults < settings.json < environment < flags.
type settings struct {
	PreferredModel        string `json:"preferred_model"`
	SystemFile            string `json:"system_file"`
	MemoryCategoriesFile  string `json:"memory_categories_file"`
	Context               int    `json:"context"`
	Heartbeat             string `json:"heartbeat"`
	RequestTimeout        string `json:"request_timeout"`
	OllamaURL             string `json:"ollama_url"`
	LlamaURL              string `json:"llama_url"`
	Debug                 bool   `json:"debug"`
	CompactTranscript     bool   `json:"compact_transcript"`
	LeanTools             bool   `json:"lean_tools"`
	AllowPlaintextPreview bool   `json:"allow_plaintext_terminal_preview"`
	WhoopCallbackListen   string `json:"whoop_callback_listen"`
	WhoopRedirectURL      string `json:"whoop_redirect_url"`
	WhoopAuthTimeout      string `json:"whoop_auth_timeout"`
	// CompactionHistory is how many session summaries the exit compaction
	// keeps; older ones are offered to the model for archiving, then deleted.
	CompactionHistory int `json:"compaction_history"`
	// CompactionInstructions is inline user guidance appended to the request;
	// CompactionInstructionsFile adds the contents of a file as well.
	CompactionInstructions     string `json:"compaction_instructions"`
	CompactionInstructionsFile string `json:"compaction_instructions_file"`
	// CompactionThinking keeps model reasoning on during compaction.
	CompactionThinking bool `json:"compaction_thinking"`
}

const settingsFile = "settings.json"

func defaultSettings() settings {
	return settings{
		Context:             16384,
		Heartbeat:           defaultHeartbeatInterval.String(),
		RequestTimeout:      (10 * time.Minute).String(),
		OllamaURL:           "http://127.0.0.1:11434/v1",
		LlamaURL:            "http://127.0.0.1:8082/v1",
		WhoopCallbackListen: "127.0.0.1:8787",
		WhoopAuthTimeout:    (10 * time.Minute).String(),
		CompactionHistory:   defaultCompactionHistory,
	}
}

func defaultSettingsPath() (string, error) {
	if value := strings.TrimSpace(os.Getenv("GEMMA4_SETTINGS")); value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemma4-heretical", settingsFile), nil
}

// loadSettings reads path on top of the defaults. A missing file yields the
// defaults with found=false; a malformed file is an error so the user sees it.
func loadSettings(path string) (settings, bool, error) {
	result := defaultSettings()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if err := json.Unmarshal(content, &result); err != nil {
		return result, true, fmt.Errorf("parse %s: %w", path, err)
	}
	if result.CompactionHistory == 0 {
		result.CompactionHistory = defaultCompactionHistory
	}
	if err := result.validate(); err != nil {
		return result, true, fmt.Errorf("%s: %w", path, err)
	}
	return result, true, nil
}

func saveSettings(path string, value settings) error {
	if err := value.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func (s settings) validate() error {
	if s.Context <= 0 {
		return errors.New("context must be greater than zero")
	}
	if s.CompactionHistory < 1 {
		return errors.New("compaction_history must be at least 1")
	}
	if _, err := parseNonNegativeDuration(s.Heartbeat); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if d, err := parseNonNegativeDuration(s.RequestTimeout); err != nil || d <= 0 {
		return errors.New("request_timeout must be a positive duration such as 10m")
	}
	if d, err := parseNonNegativeDuration(s.WhoopAuthTimeout); err != nil || d <= 0 {
		return errors.New("whoop_auth_timeout must be a positive duration such as 10m")
	}
	for name, value := range map[string]string{"ollama_url": s.OllamaURL, "llama_url": s.LlamaURL} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	return nil
}

func (s settings) heartbeat() time.Duration {
	d, _ := parseNonNegativeDuration(s.Heartbeat)
	return d
}

func (s settings) requestTimeout() time.Duration {
	d, _ := parseNonNegativeDuration(s.RequestTimeout)
	return d
}

func (s settings) whoopAuthTimeout() time.Duration {
	d, _ := parseNonNegativeDuration(s.WhoopAuthTimeout)
	return d
}

// parseNonNegativeDuration accepts Go durations and a bare "0".
func parseNonNegativeDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 15m or 2h", value)
	}
	if d < 0 {
		return 0, errors.New("duration cannot be negative")
	}
	return d, nil
}

// applyEnvironment layers the environment overrides that predate
// settings.json on top of stored settings.
func (s settings) applyEnvironment(getenv func(string) string) (settings, error) {
	if value := strings.TrimSpace(getenv("GEMMA4_CONTEXT")); value != "" {
		parsed, err := parseIntEnv("GEMMA4_CONTEXT", value)
		if err != nil {
			return s, err
		}
		s.Context = parsed
	}
	if value := strings.TrimSpace(getenv("GEMMA4_MEMORY_CATEGORIES")); value != "" {
		s.MemoryCategoriesFile = value
	}
	if value := strings.TrimSpace(getenv("GEMMA4_OLLAMA_URL")); value != "" {
		s.OllamaURL = value
	}
	if value := strings.TrimSpace(getenv("GEMMA4_LLAMA_URL")); value != "" {
		s.LlamaURL = value
	}
	if value := strings.TrimSpace(getenv("WHOOP_CALLBACK_LISTEN")); value != "" {
		s.WhoopCallbackListen = value
	}
	if value := strings.TrimSpace(getenv("WHOOP_REDIRECT_URL")); value != "" {
		s.WhoopRedirectURL = value
	}
	return s, nil
}

// modelKey is the stable "backend/model" form stored in preferred_model.
func modelKey(choice modelChoice) string {
	return string(choice.Backend) + "/" + choice.Model
}

// toConfig expands settings into the runtime config, keeping any per-run
// fields that live only in appConfig.
func (s settings) toConfig(base appConfig) appConfig {
	base.Settings = s
	base.PreferredModel = s.PreferredModel
	base.SystemFile = s.SystemFile
	base.MemoryCategoriesFile = strings.TrimSpace(s.MemoryCategoriesFile)
	base.Context = s.Context
	base.Heartbeat = s.heartbeat()
	base.RequestTimeout = s.requestTimeout()
	base.OllamaBaseURL = strings.TrimSuffix(strings.TrimSpace(s.OllamaURL), "/")
	base.LlamaBaseURL = strings.TrimSuffix(strings.TrimSpace(s.LlamaURL), "/")
	base.Debug = s.Debug
	base.CompactTranscript = s.CompactTranscript
	base.LeanTools = s.LeanTools
	base.AllowPlaintextPreview = s.AllowPlaintextPreview
	base.WhoopCallbackListen = strings.TrimSpace(s.WhoopCallbackListen)
	base.WhoopRedirectURL = strings.TrimSpace(s.WhoopRedirectURL)
	base.WhoopAuthTimeout = s.whoopAuthTimeout()
	base.CompactionHistory = s.CompactionHistory
	base.CompactionInstructions = strings.TrimSpace(s.CompactionInstructions)
	base.CompactionInstructionsFile = strings.TrimSpace(s.CompactionInstructionsFile)
	base.CompactionThinking = s.CompactionThinking
	return base
}
