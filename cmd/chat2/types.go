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
	Role             string     `json:"role"`
	Content          any        `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type usage struct {
	PromptTokens int `json:"prompt_tokens"`
}

type imageRequest struct {
	ToolCallID string
	Prompt     string
	Reason     string
}

type transcriptMessage struct {
	Role       string
	Content    string
	Images     []string
	OccurredAt time.Time
	Meta       string
}

type memoryEntry struct {
	Timestamp string   `json:"timestamp"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags,omitempty"`
}

type recentMessage struct {
	Timestamp string `json:"timestamp"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

type appConfig struct {
	Context               int
	Heartbeat             time.Duration
	Debug                 bool
	SystemFile            string
	StartupImages         []string
	OllamaBaseURL         string
	LlamaBaseURL          string
	RequestTimeout        time.Duration
	AllowPlaintextPreview bool
	WhoopAuth             bool
	WhoopCallbackListen   string
	WhoopRedirectURL      string
	WhoopAuthTimeout      time.Duration
}
