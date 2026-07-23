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
	if model, ok := finalModel.(appModel); ok {
		cleanupErr = model.cleanupOwnedImages()
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
	var images imageFlags
	defaultContext, err := envInt("GEMMA4_CONTEXT", 16384)
	if err != nil {
		return appConfig{}, err
	}

	contextSize := flag.Int("context", defaultContext, "context window used for Ollama requests and the context meter")
	heartbeat := flag.Duration("heartbeat", defaultHeartbeatInterval, "heartbeat interval; use 0 to disable")
	debug := flag.Bool("debug", false, "show harness and system messages in the transcript")
	systemFile := flag.String("system-file", "", "file to inject as the highest-priority system prompt")
	ollamaURL := flag.String("ollama-url", envOr("GEMMA4_OLLAMA_URL", "http://127.0.0.1:11434/v1"), "Ollama OpenAI-compatible base URL")
	llamaURL := flag.String("llama-url", envOr("GEMMA4_LLAMA_URL", "http://127.0.0.1:8082/v1"), "llama.cpp OpenAI-compatible base URL")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-request timeout")
	allowPlaintextPreview := flag.Bool("allow-plaintext-terminal-preview", false, "allow ImageSnap terminal previews, which briefly write plaintext frames")
	whoopAuth := flag.Bool("whoop-auth", false, "authorize Whoop access and save the OAuth token, then exit")
	whoopCallbackListen := flag.String("whoop-callback-listen", envOr("WHOOP_CALLBACK_LISTEN", "127.0.0.1:8787"), "local address receiving the Whoop callback forwarded by ngrok")
	whoopRedirectURL := flag.String("whoop-redirect-url", envOr("WHOOP_REDIRECT_URL", ""), "public HTTPS Whoop callback URL registered in the developer dashboard")
	whoopAuthTimeout := flag.Duration("whoop-auth-timeout", 10*time.Minute, "maximum time to wait for the Whoop OAuth callback")
	flag.Var(&images, "image", "image to queue for the first message (repeatable)")
	flag.Parse()

	if flag.NArg() > 1 {
		return appConfig{}, fmt.Errorf("at most one positional system file is allowed")
	}
	if flag.NArg() == 1 && *systemFile == "" {
		*systemFile = flag.Arg(0)
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

	return appConfig{
		Context:               *contextSize,
		Heartbeat:             *heartbeat,
		Debug:                 *debug,
		SystemFile:            *systemFile,
		StartupImages:         images,
		OllamaBaseURL:         strings.TrimSuffix(*ollamaURL, "/"),
		LlamaBaseURL:          strings.TrimSuffix(*llamaURL, "/"),
		RequestTimeout:        *timeout,
		AllowPlaintextPreview: *allowPlaintextPreview,
		WhoopAuth:             *whoopAuth,
		WhoopCallbackListen:   strings.TrimSpace(*whoopCallbackListen),
		WhoopRedirectURL:      strings.TrimSpace(*whoopRedirectURL),
		WhoopAuthTimeout:      *whoopAuthTimeout,
	}, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}
