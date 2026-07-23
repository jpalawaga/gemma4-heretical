package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const responseOptOutToken = "<response-opt-out>"

func isResponseOptOut(content string) bool {
	return strings.TrimSpace(content) == responseOptOutToken
}

func isPotentialResponseOptOut(content string) bool {
	trimmed := strings.TrimSpace(content)
	return trimmed == "" || strings.HasPrefix(responseOptOutToken, trimmed)
}

type completionStreamEventKind int

const (
	streamConnected completionStreamEventKind = iota
	streamReasoningDelta
	streamContentDelta
	streamToolRound
	streamFinished
)

type completionStreamEvent struct {
	Kind      completionStreamEventKind
	Delta     string
	Reasoning string
	Content   string
	ToolNames []string
	Result    completionResult
	Err       error
}

type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Role             string          `json:"role"`
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage usage `json:"usage"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolCallBuilder struct {
	ID        string
	Type      string
	Name      strings.Builder
	Arguments strings.Builder
}

func (c *apiClient) completeStream(ctx context.Context, messages []apiMessage, allowHeartbeatScheduling bool) <-chan completionStreamEvent {
	events := make(chan completionStreamEvent, 64)
	go func() {
		defer close(events)
		c.mu.Lock()
		defer c.mu.Unlock()

		emit := func(event completionStreamEvent) bool {
			select {
			case events <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		result, err := c.completeStreaming(ctx, messages, allowHeartbeatScheduling, emit)
		final := completionStreamEvent{Kind: streamFinished, Result: result, Err: err}
		if ctx.Err() == nil {
			events <- final
		} else {
			// After explicit UI cancellation there may no longer be a consumer.
			select {
			case events <- final:
			default:
			}
		}
	}()
	return events
}

func (c *apiClient) complete(ctx context.Context, messages []apiMessage) (completionResult, error) {
	for event := range c.completeStream(ctx, messages, true) {
		if event.Kind == streamFinished {
			return event.Result, event.Err
		}
	}
	return completionResult{}, errors.New("completion stream closed without a result")
}

func (c *apiClient) completeStreaming(
	ctx context.Context,
	messages []apiMessage,
	allowHeartbeatScheduling bool,
	emit func(completionStreamEvent) bool,
) (completionResult, error) {
	working := append([]apiMessage(nil), messages...)
	var notices []string
	hadToolCalls := false
	hadVisibleReply := false
	c.nextHeartbeat = nil
	c.heartbeatSchedulingAllowed = allowHeartbeatScheduling
	defer func() { c.heartbeatSchedulingAllowed = false }()
	for range 8 {
		request := chatRequest{
			Model:         c.choice.Model,
			Messages:      working,
			Tools:         c.completionTools(allowHeartbeatScheduling),
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

		message, roundUsage, err := c.streamChat(ctx, request, emit)
		if err != nil {
			return completionResult{}, err
		}
		reply, _ := message.Content.(string)
		if allowHeartbeatScheduling && len(message.ToolCalls) == 0 {
			if visibleReply, when, ok := extractTrailingHeartbeatDirective(reply, time.Now()); ok {
				reply = visibleReply
				c.nextHeartbeat = &when
				notices = append(notices, "next heartbeat "+when.Format("2006-01-02 15:04 MST"))
			}
		}
		if strings.TrimSpace(reply) != "" && (allowHeartbeatScheduling || !isResponseOptOut(reply)) {
			hadVisibleReply = true
		}
		if len(message.ToolCalls) == 0 {
			finalReply := reply
			if !allowHeartbeatScheduling && isResponseOptOut(finalReply) {
				finalReply = ""
			}
			return completionResult{
				Reply:           finalReply,
				Reasoning:       message.ReasoningContent,
				Usage:           roundUsage,
				Messages:        working,
				Notices:         notices,
				NextHeartbeat:   c.nextHeartbeat,
				HadToolCalls:    hadToolCalls,
				HadVisibleReply: hadVisibleReply,
			}, nil
		}
		hadToolCalls = true

		// Heartbeat timing is response metadata rather than a tool result the
		// model needs to interpret. When it accompanies a visible response, apply
		// it and finish this turn without paying for another completion round.
		var precomputedHeartbeatResult string
		if len(message.ToolCalls) == 1 &&
			message.ToolCalls[0].Function.Name == "schedule_next_heartbeat" &&
			strings.TrimSpace(reply) != "" {
			call := message.ToolCalls[0]
			if call.ID == "" {
				return completionResult{}, fmt.Errorf("streamed tool call %q has no ID", call.Function.Name)
			}
			result, notice := c.runTool(call)
			if notice != "" {
				notices = append(notices, notice)
				return completionResult{
					Reply:           reply,
					Reasoning:       message.ReasoningContent,
					Usage:           roundUsage,
					Messages:        working,
					Notices:         notices,
					NextHeartbeat:   c.nextHeartbeat,
					HadToolCalls:    hadToolCalls,
					HadVisibleReply: hadVisibleReply,
				}, nil
			}
			// Let the model see an invalid scheduling result and repair it in the
			// ordinary tool loop.
			precomputedHeartbeatResult = result
		}

		working = append(working, message)
		toolNames := make([]string, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			toolNames = append(toolNames, call.Function.Name)
		}
		if !emit(completionStreamEvent{
			Kind:      streamToolRound,
			Reasoning: message.ReasoningContent,
			Content:   reply,
			ToolNames: toolNames,
		}) {
			return completionResult{}, ctx.Err()
		}

		var pendingRequest *imageRequest
		for callIndex, call := range message.ToolCalls {
			if call.ID == "" {
				return completionResult{}, fmt.Errorf("streamed tool call %q has no ID", call.Function.Name)
			}
			if call.Function.Name == "request_image" {
				request, err := parseImageRequest(call)
				if err != nil {
					working = append(working, apiMessage{
						Role:       "tool",
						ToolCallID: call.ID,
						Content:    "The image request was invalid: " + err.Error(),
					})
					continue
				}
				if pendingRequest == nil {
					pendingRequest = request
					continue
				}
				working = append(working, apiMessage{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    "Only one image can be requested at a time.",
				})
				continue
			}
			result, notice := "", ""
			if callIndex == 0 && precomputedHeartbeatResult != "" {
				result = precomputedHeartbeatResult
			} else {
				result, notice = c.runTool(call)
			}
			if notice != "" {
				notices = append(notices, notice)
			}
			working = append(working, apiMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    result,
			})
		}
		if pendingRequest != nil {
			return completionResult{
				Messages:        working,
				Notices:         notices,
				ImageRequest:    pendingRequest,
				Usage:           roundUsage,
				NextHeartbeat:   c.nextHeartbeat,
				HadToolCalls:    hadToolCalls,
				HadVisibleReply: hadVisibleReply,
			}, nil
		}
	}
	return completionResult{}, errors.New("tool call limit exceeded")
}

func (c *apiClient) completionTools(allowHeartbeatScheduling bool) []toolSpec {
	available := make([]toolSpec, 0, len(tools))
	for _, tool := range tools {
		if tool.Function.Name == "schedule_next_heartbeat" && !allowHeartbeatScheduling {
			continue
		}
		if tool.Function.Name == "get_whoop_recovery_sleep" && (c.whoop == nil || !c.whoop.Configured(time.Now())) {
			continue
		}
		available = append(available, tool)
	}
	return available
}

func extractTrailingHeartbeatDirective(content string, now time.Time) (string, time.Time, bool) {
	trimmed := strings.TrimRight(content, " \t\r\n")
	lineStart := strings.LastIndexByte(trimmed, '\n') + 1
	if lineStart <= 0 {
		return content, time.Time{}, false
	}
	visible := strings.TrimSpace(trimmed[:lineStart])
	if visible == "" {
		return content, time.Time{}, false
	}
	candidate := strings.TrimSpace(trimmed[lineStart:])
	value, ok := printedHeartbeatWhen(candidate)
	if !ok {
		return content, time.Time{}, false
	}
	when, err := parseNextHeartbeat(value, now)
	if err != nil {
		return content, time.Time{}, false
	}
	return visible, when, true
}

func printedHeartbeatWhen(candidate string) (string, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(candidate), &object) == nil && len(object) == 1 {
		rawWhen, exists := object["when"]
		if !exists {
			return "", false
		}
		var value string
		if json.Unmarshal(rawWhen, &value) != nil {
			return "", false
		}
		return value, true
	}

	const callName = "schedule_next_heartbeat"
	if !strings.HasPrefix(candidate, callName+"(") || !strings.HasSuffix(candidate, ")") {
		return "", false
	}
	arguments := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(candidate, callName+"("), ")"))
	name, rawValue, found := strings.Cut(arguments, "=")
	rawValue = strings.TrimSpace(rawValue)
	if !found || strings.TrimSpace(name) != "when" || rawValue == "" {
		return "", false
	}
	quote := rawValue[0]
	if quote != '\'' && quote != '"' {
		if strings.ContainsAny(rawValue, " \t\r\n,\\'\"") {
			return "", false
		}
		return rawValue, true
	}
	if len(rawValue) < 2 || rawValue[len(rawValue)-1] != quote {
		return "", false
	}
	value := rawValue[1 : len(rawValue)-1]
	if value == "" || strings.ContainsRune(value, rune(quote)) || strings.ContainsRune(value, '\\') {
		return "", false
	}
	return value, true
}

func hidePotentialTrailingHeartbeatDirective(content string) string {
	lineStart := strings.LastIndexByte(content, '\n') + 1
	if lineStart <= 0 || strings.TrimSpace(content[:lineStart]) == "" {
		return content
	}
	candidate := strings.TrimSpace(content[lineStart:])
	for _, toolPrefix := range []string{`{"when"`, "schedule_next_heartbeat("} {
		if candidate != "" && (strings.HasPrefix(toolPrefix, candidate) || strings.HasPrefix(candidate, toolPrefix)) {
			return strings.TrimRight(content[:lineStart], " \t\r\n")
		}
	}
	return content
}

func (c *apiClient) streamChat(
	ctx context.Context,
	payload chatRequest,
	emit func(completionStreamEvent) bool,
) (apiMessage, usage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return apiMessage{}, usage{}, err
	}
	defer clear(body)
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimSuffix(c.choice.BaseURL, "/")+"/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return apiMessage{}, usage{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+c.choice.APIKey)
	response, err := c.http.Do(request)
	if err != nil {
		return apiMessage{}, usage{}, fmt.Errorf("%s is unavailable: %w", c.choice.Backend, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return apiMessage{}, usage{}, fmt.Errorf("%s returned %s: %s", c.choice.Backend, response.Status, strings.TrimSpace(string(message)))
	}
	if !emit(completionStreamEvent{Kind: streamConnected}) {
		return apiMessage{}, usage{}, ctx.Err()
	}

	var (
		content       strings.Builder
		reasoning     strings.Builder
		builders      = map[int]*toolCallBuilder{}
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
			for _, call := range delta.ToolCalls {
				builder := builders[call.Index]
				if builder == nil {
					builder = &toolCallBuilder{}
					builders[call.Index] = builder
				}
				if call.ID != "" {
					builder.ID = call.ID
				}
				if call.Type != "" {
					builder.Type = call.Type
				}
				builder.Name.WriteString(call.Function.Name)
				builder.Arguments.WriteString(call.Function.Arguments)
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
		return apiMessage{}, usage{}, processingErr
	}
	if err := scanner.Err(); err != nil {
		return apiMessage{}, usage{}, fmt.Errorf("read completion stream: %w", err)
	}
	if !sawJSON {
		return apiMessage{}, usage{}, errors.New("completion stream contained no JSON events")
	}

	indexes := make([]int, 0, len(builders))
	for index := range builders {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	toolCalls := make([]toolCall, 0, len(indexes))
	for _, index := range indexes {
		builder := builders[index]
		toolCalls = append(toolCalls, toolCall{
			ID:   builder.ID,
			Type: builder.Type,
			Function: functionCall{
				Name:      builder.Name.String(),
				Arguments: builder.Arguments.String(),
			},
		})
	}
	return apiMessage{
		Role:             "assistant",
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        toolCalls,
	}, streamUsage, nil
}
