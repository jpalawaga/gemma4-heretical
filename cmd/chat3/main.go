// Command chat3 is a minimal Charm TUI chat client for the local Ollama and
// llama.cpp OpenAI-compatible APIs. Unlike chat2 it has no tools, memory,
// conversation persistence, scheduling, heartbeats, or startup protocol: it
// discovers models, lets you pick one, and streams plain chat completions.
package main

import (
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
		fmt.Fprintln(os.Stderr, "chat3:", err)
		os.Exit(2)
	}
	for index, path := range cfg.StartupImages {
		resolved, err := resolveImage(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "chat3:", err)
			os.Exit(2)
		}
		cfg.StartupImages[index] = resolved
	}
	if cfg.SystemFile != "" {
		if _, err := os.Stat(cfg.SystemFile); err != nil {
			fmt.Fprintln(os.Stderr, "chat3: system file:", err)
			os.Exit(2)
		}
	}

	if _, err := tea.NewProgram(newAppModel(cfg)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "chat3:", err)
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
	debug := flag.Bool("debug", false, "show reasoning and system messages in the transcript")
	systemFile := flag.String("system-file", "", "file to send as the system prompt")
	ollamaURL := flag.String("ollama-url", envOr("GEMMA4_OLLAMA_URL", "http://127.0.0.1:11434/v1"), "Ollama OpenAI-compatible base URL")
	llamaURL := flag.String("llama-url", envOr("GEMMA4_LLAMA_URL", "http://127.0.0.1:8082/v1"), "llama.cpp OpenAI-compatible base URL")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-request timeout")
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
	if *timeout <= 0 {
		return appConfig{}, fmt.Errorf("--timeout must be greater than zero")
	}

	return appConfig{
		Context:        *contextSize,
		Debug:          *debug,
		SystemFile:     *systemFile,
		StartupImages:  images,
		OllamaBaseURL:  strings.TrimSuffix(*ollamaURL, "/"),
		LlamaBaseURL:   strings.TrimSuffix(*llamaURL, "/"),
		RequestTimeout: *timeout,
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
