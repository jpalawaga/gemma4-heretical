package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type imageFlags []string

func (f *imageFlags) String() string {
	return strings.Join(*f, ",")
}

func (f *imageFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chat2:", err)
		os.Exit(2)
	}
	memory, err := newMemoryStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chat2:", err)
		os.Exit(1)
	}
	memory.categoriesFile = cfg.MemoryCategoriesFile
	if cfg.WhoopAuth {
		if err := runWhoopAuthWithNgrok(context.Background(), newWhoopClient(whoopConfigFromEnvironment(memory.dir)), cfg); err != nil {
			fmt.Fprintln(os.Stderr, "chat2: Whoop authorization:", err)
			os.Exit(1)
		}
		return
	}
	for index, path := range cfg.StartupImages {
		resolved, err := resolveImage(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "chat2:", err)
			os.Exit(2)
		}
		cfg.StartupImages[index] = resolved
	}

	program := tea.NewProgram(newAppModel(cfg, memory))
	finalModel, runErr := program.Run()
	var cleanupErr error
	var exitNotice string
	if model, ok := finalModel.(appModel); ok {
		cleanupErr = model.cleanupOwnedImages()
		exitNotice = model.exitNotice
	}
	if exitNotice != "" {
		fmt.Println("chat2:", exitNotice)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "chat2:", runErr)
	}
	if cleanupErr != nil {
		fmt.Fprintln(os.Stderr, "chat2:", cleanupErr)
	}
	if runErr != nil || cleanupErr != nil {
		os.Exit(1)
	}
}

func parseConfig() (appConfig, error) {
	settingsPath, err := defaultSettingsPath()
	if err != nil {
		return appConfig{}, err
	}
	return parseConfigFrom(os.Args[1:], os.Getenv, settingsPath)
}

// parseConfigFrom layers configuration as defaults < settings.json <
// environment < flags. Flag defaults are seeded from the earlier layers, so
// any flag the user passes wins without separate bookkeeping.
func parseConfigFrom(args []string, getenv func(string) string, settingsPath string) (appConfig, error) {
	stored, _, err := loadSettings(settingsPath)
	if err != nil {
		return appConfig{}, err
	}
	base, err := stored.applyEnvironment(getenv)
	if err != nil {
		return appConfig{}, err
	}

	var images imageFlags
	flags := flag.NewFlagSet("chat2", flag.ContinueOnError)
	contextSize := flags.Int("context", base.Context, "context window used for Ollama requests and the context meter")
	heartbeat := flags.Duration("heartbeat", base.heartbeat(), "heartbeat interval; use 0 to disable")
	debug := flags.Bool("debug", base.Debug, "show harness and system messages in the transcript")
	compact := flags.Bool("compact", base.CompactTranscript, "start with the compact transcript")
	systemFile := flags.String("system-file", base.SystemFile, "file to inject as the highest-priority system prompt")
	memoryCategoriesFile := flags.String("memory-categories", base.MemoryCategoriesFile, "memory categories file (default: memory-categories.txt in the data directory)")
	preferredModel := flags.String("model", base.PreferredModel, "connect to this backend/model automatically when it is online")
	ollamaURL := flags.String("ollama-url", base.OllamaURL, "Ollama OpenAI-compatible base URL")
	llamaURL := flags.String("llama-url", base.LlamaURL, "llama.cpp OpenAI-compatible base URL")
	timeout := flags.Duration("timeout", base.requestTimeout(), "per-request timeout")
	allowPlaintextPreview := flags.Bool("allow-plaintext-terminal-preview", base.AllowPlaintextPreview, "allow ImageSnap terminal previews, which briefly write plaintext frames")
	leanTools := flags.Bool("lean-tools", base.LeanTools, "hide scheduling, memory-admin, images, and whoop tools until the model requests them with request_toolcall")
	whoopAuth := flags.Bool("whoop-auth", false, "authorize Whoop access and save the OAuth token, then exit")
	whoopCallbackListen := flags.String("whoop-callback-listen", base.WhoopCallbackListen, "local address receiving the Whoop callback forwarded by ngrok")
	whoopRedirectURL := flags.String("whoop-redirect-url", base.WhoopRedirectURL, "public HTTPS Whoop callback URL registered in the developer dashboard")
	whoopAuthTimeout := flags.Duration("whoop-auth-timeout", base.whoopAuthTimeout(), "maximum time to wait for the Whoop OAuth callback")
	flags.Var(&images, "image", "image to queue for the first message (repeatable)")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: chat2 [flags] [system-file]\n\nSettings persist in %s and can be edited in the app with F2 or /settings.\nFlags override the stored settings for one run.\n\n", settingsPath)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return appConfig{}, err
	}

	if flags.NArg() > 1 {
		return appConfig{}, fmt.Errorf("at most one positional system file is allowed")
	}
	if flags.NArg() == 1 {
		*systemFile = flags.Arg(0)
	}
	if *contextSize <= 0 {
		return appConfig{}, fmt.Errorf("--context must be greater than zero")
	}
	if *heartbeat < 0 {
		return appConfig{}, fmt.Errorf("--heartbeat cannot be negative")
	}
	if *timeout <= 0 {
		return appConfig{}, fmt.Errorf("--timeout must be greater than zero")
	}
	if *whoopAuthTimeout <= 0 {
		return appConfig{}, fmt.Errorf("--whoop-auth-timeout must be greater than zero")
	}

	effective := base
	effective.PreferredModel = strings.TrimSpace(*preferredModel)
	effective.SystemFile = strings.TrimSpace(*systemFile)
	effective.MemoryCategoriesFile = strings.TrimSpace(*memoryCategoriesFile)
	effective.Context = *contextSize
	effective.Heartbeat = durationSetting(*heartbeat)
	effective.RequestTimeout = timeout.String()
	effective.OllamaURL = *ollamaURL
	effective.LlamaURL = *llamaURL
	effective.Debug = *debug
	effective.CompactTranscript = *compact
	effective.LeanTools = *leanTools
	effective.AllowPlaintextPreview = *allowPlaintextPreview
	effective.WhoopCallbackListen = *whoopCallbackListen
	effective.WhoopRedirectURL = *whoopRedirectURL
	effective.WhoopAuthTimeout = whoopAuthTimeout.String()
	if err := effective.validate(); err != nil {
		return appConfig{}, err
	}
	cfg := effective.toConfig(appConfig{
		SettingsPath:  settingsPath,
		StartupImages: images,
		WhoopAuth:     *whoopAuth,
	})
	return cfg, nil
}

func durationSetting(value time.Duration) string {
	if value <= 0 {
		return "0"
	}
	return value.String()
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func parseIntEnv(name, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}
