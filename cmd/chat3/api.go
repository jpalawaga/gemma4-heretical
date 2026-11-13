package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type apiClient struct {
	http    *http.Client
	choice  modelChoice
	context int
}

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []apiMessage   `json:"messages"`
	Options         map[string]any `json:"options,omitempty"`
	KeepAlive       any            `json:"keep_alive,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   map[string]any `json:"stream_options,omitempty"`
	ReasoningFormat string         `json:"reasoning_format,omitempty"`
}

type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage usage `json:"usage"`
}

type completionResult struct {
	Reply     string
	Reasoning string
	Usage     usage
}

type completionStreamEventKind int

const (
	streamConnected completionStreamEventKind = iota
	streamReasoningDelta
	streamContentDelta
	streamFinished
)

type completionStreamEvent struct {
	Kind   completionStreamEventKind
	Delta  string
	Result completionResult
	Err    error
}

func newAPIClient(choice modelChoice, contextSize int, timeout time.Duration) *apiClient {
	return &apiClient{
		http:    &http.Client{Timeout: timeout},
		choice:  choice,
		context: contextSize,
	}
}

func discoverModels(ctx context.Context, cfg appConfig) []modelChoice {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		choices []modelChoice
	)

	add := func(found []modelChoice) {
		mu.Lock()
		choices = append(choices, found...)
		mu.Unlock()
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		add(discoverOllama(ctx, cfg.OllamaBaseURL))
	}()
	go func() {
		defer wg.Done()
		add(discoverOpenAI(ctx, cfg.LlamaBaseURL, backendLlama))
	}()
	wg.Wait()

	hasBackend := func(backend backendKind) bool {
		for _, choice := range choices {
			if choice.Backend == backend {
				return true
			}
		}
		return false
	}
	if !hasBackend(backendLlama) {
		choices = append(choices, modelChoice{
			Backend: backendLlama,
			Model:   "gemma4-heretical",
			BaseURL: strings.TrimSuffix(cfg.LlamaBaseURL, "/"),
			APIKey:  "none",
			Detail:  "start ./serve-llama to connect",
		})
	}
	if !hasBackend(backendOllama) {
		choices = append(choices, modelChoice{
			Backend: backendOllama,
			Model:   "gemma4-heretical",
			BaseURL: strings.TrimSuffix(cfg.OllamaBaseURL, "/"),
			APIKey:  "ollama",
			Detail:  "start Ollama to connect",
		})
	}

	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].Online != choices[j].Online {
			return choices[i].Online
		}
		if choices[i].Backend != choices[j].Backend {
			return choices[i].Backend < choices[j].Backend
		}
		return choices[i].Model < choices[j].Model
	})

	return choices
}

func discoverOllama(ctx context.Context, baseURL string) []modelChoice {
	apiRoot := strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1")
	var payload struct {
		Models []struct {
			Name    string `json:"name"`
			Details struct {
				ParameterSize     string `json:"parameter_size"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := getJSON(ctx, apiRoot+"/api/tags", &payload); err != nil {
		return nil
	}

	result := make([]modelChoice, 0, len(payload.Models))
	for _, model := range payload.Models {
		detail := "local Ollama model"
		var pieces []string
		if model.Details.ParameterSize != "" {
			pieces = append(pieces, model.Details.ParameterSize)
		}
		if model.Details.QuantizationLevel != "" {
			pieces = append(pieces, model.Details.QuantizationLevel)
		}
		if len(pieces) > 0 {
			detail = strings.Join(pieces, " · ")
		}
		result = append(result, modelChoice{
			Backend: backendOllama,
			Model:   model.Name,
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			APIKey:  "ollama",
			Online:  true,
			Detail:  detail,
		})
	}
	return result
}

func discoverOpenAI(ctx context.Context, baseURL string, backend backendKind) []modelChoice {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, strings.TrimSuffix(baseURL, "/")+"/models", &payload); err != nil {
		return nil
	}
	result := make([]modelChoice, 0, len(payload.Data))
	for _, model := range payload.Data {
		result = append(result, modelChoice{
			Backend: backend,
			Model:   model.ID,
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			APIKey:  "none",
			Online:  true,
			Detail:  "vision + MTP server",
		})
	}
	return result
}

func getJSON(ctx context.Context, url string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 1800 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target)
}

func imageParts(prompt string, paths []string) ([]contentPart, error) {
	parts := []contentPart{{Type: "text", Text: prompt}}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read image %q: %w", path, err)
		}
		mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
		if mediaType == "" {
			return nil, fmt.Errorf("unsupported image extension: %s", filepath.Ext(path))
		}
		parts = append(parts, contentPart{
			Type: "image_url",
			ImageURL: &imageURL{
				URL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	return parts, nil
}

// completeStream runs one streamed chat completion and delivers its progress
// on the returned channel. The channel is closed after the streamFinished
// event, which always carries either a result or an error.
func (c *apiClient) completeStream(ctx context.Context, messages []apiMessage) <-chan completionStreamEvent {
	events := make(chan completionStreamEvent, 64)
	go func() {
		defer close(events)
		emit := func(event completionStreamEvent) bool {
			select {
			case events <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		request := chatRequest{
			Model:         c.choice.Model,
			Messages:      messages,
			Stream:        true,
			StreamOptions: map[string]any{"include_usage": true},
		}
		if c.choice.Backend == backendLlama {
			request.ReasoningFormat = "deepseek"
		}
		if c.choice.Backend == backendOllama {
			request.Options = map[string]any{"num_ctx": c.context}
			request.KeepAlive = -1
		}
		result, err := c.streamChat(ctx, request, emit)
		final := completionStreamEvent{Kind: streamFinished, Result: result, Err: err}
		select {
		case events <- final:
		case <-ctx.Done():
			// After UI cancellation there may no longer be a consumer.
			select {
			case events <- final:
			default:
			}
		}
	}()
	return events
}

func (c *apiClient) streamChat(
	ctx context.Context,
	payload chatRequest,
	emit func(completionStreamEvent) bool,
) (completionResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return completionResult{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimSuffix(c.choice.BaseURL, "/")+"/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return completionResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+c.choice.APIKey)
	response, err := c.http.Do(request)
	if err != nil {
		return completionResult{}, fmt.Errorf("%s is unavailable: %w", c.choice.Backend, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return completionResult{}, fmt.Errorf("%s returned %s: %s", c.choice.Backend, response.Status, strings.TrimSpace(string(message)))
	}
	if !emit(completionStreamEvent{Kind: streamConnected}) {
		return completionResult{}, ctx.Err()
	}

	var (
		content       strings.Builder
		reasoning     strings.Builder
		streamUsage   usage
		sawJSON       bool
		dataLines     []string
		processingErr error
	)
	processEvent := func() bool {
		if len(dataLines) == 0 {
			return true
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if strings.TrimSpace(data) == "[DONE]" {
			return false
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			processingErr = fmt.Errorf("decode completion stream event: %w", err)
			return false
		}
		sawJSON = true
		if chunk.Usage.PromptTokens > 0 {
			streamUsage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if delta.ReasoningContent != "" {
				reasoning.WriteString(delta.ReasoningContent)
				if !emit(completionStreamEvent{Kind: streamReasoningDelta, Delta: delta.ReasoningContent}) {
					processingErr = ctx.Err()
					return false
				}
			}
			if delta.Content != "" {
				content.WriteString(delta.Content)
				if !emit(completionStreamEvent{Kind: streamContentDelta, Delta: delta.Content}) {
					processingErr = ctx.Err()
					return false
				}
			}
		}
		return true
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	keepReading := true
	for keepReading && scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			keepReading = processEvent()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if keepReading && len(dataLines) > 0 {
		processEvent()
	}
	if processingErr != nil {
		return completionResult{}, processingErr
	}
	if err := scanner.Err(); err != nil {
		return completionResult{}, fmt.Errorf("read completion stream: %w", err)
	}
	if !sawJSON {
		return completionResult{}, errors.New("completion stream contained no JSON events")
	}
	return completionResult{
		Reply:     content.String(),
		Reasoning: reasoning.String(),
		Usage:     streamUsage,
	}, nil
}
