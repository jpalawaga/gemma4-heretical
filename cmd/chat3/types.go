package main

import "time"

type backendKind string

const (
	backendOllama backendKind = "ollama"
	backendLlama  backendKind = "llama.cpp"
)

type modelChoice struct {
	Backend backendKind
	Model   string
	BaseURL string
	APIKey  string
	Online  bool
	Detail  string
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type apiMessage struct {
	Role             string `json:"role"`
	Content          any    `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type usage struct {
	PromptTokens int `json:"prompt_tokens"`
}

type transcriptMessage struct {
	Role       string
	Content    string
	Images     []string
	OccurredAt time.Time
	Meta       string
}

type appConfig struct {
	Context        int
	Debug          bool
	SystemFile     string
	StartupImages  []string
	OllamaBaseURL  string
	LlamaBaseURL   string
	RequestTimeout time.Duration
}
