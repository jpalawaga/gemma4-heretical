package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Exit compaction. When the user quits, the model reviews the session with
// its memory and schedule tools, archives anything worth keeping from session
// summaries that are about to fall out of the retained history, and finishes
// with a complete_compaction call carrying the handoff summary for this
// session. The summary is appended to the datestamped compaction history.

var completeCompactionTool = toolSpec{
	Type: "function",
	Function: functionSpec{
		Name:        "complete_compaction",
		Description: "Finish the exit compaction with the handoff summary of this session. Call this exactly once, after any memory or schedule tool calls. Prose outside this call is discarded.",
		Parameters: objectSchema(
			map[string]any{
				"summary": stringSchema("The complete handoff summary of this session, at most 250 words."),
			},
			[]string{"summary"},
		),
	},
}

type compactionRequest struct {
	// Dropping lists older session summaries that will be deleted once this
	// compaction is stored, so the model can archive what still matters.
	Dropping []compactedConversation
	// Instructions is optional user-supplied guidance appended to the request.
	Instructions string
	// Thinking keeps the model's reasoning enabled for the compaction. Off by
	// default because it multiplies the time the exit takes.
	Thinking bool
	// MidSession means the conversation continues after compaction: the
	// summary replaces the earlier turns in context instead of closing out.
	MidSession bool
}

// historyTrimNotePrefix marks the system note inserted when the oldest turns
// had to be dropped to fit the context window.
const historyTrimNotePrefix = "[HARNESS NOTE] The earliest "

func compactionPrompt(request compactionRequest) string {
	var prompt strings.Builder
	if request.MidSession {
		prompt.WriteString(`[HARNESS REQUEST: compact_conversation]
The conversation is continuing, but its history is being compacted to free context. You have the conversation so far, your saved memories, and your memory and schedule tools. Do the following, then finish with exactly one complete_compaction call.
`)
	} else {
		prompt.WriteString(`[HARNESS REQUEST: compact_conversation]
This session is ending. You have the full conversation, your saved memories, and your memory and schedule tools. Do the following, then finish with exactly one complete_compaction call.
`)
	}
	prompt.WriteString(`
1. Review this session for anything the in-turn responses failed to persist. Durable facts, preferences, decisions, and commitments belong in memory (save_memory, or update_memory to refine an existing entry). Dated tasks, reminders, and follow-ups belong in the schedule (schedule_item). Skip anything already saved or scheduled, and do not invent details.`)
	if len(request.Dropping) > 0 {
		prompt.WriteString(`

2. The following older session summaries are about to be deleted from the compaction history. Save any detail from them that is still worth keeping and is not already in memory as memory entries, then let the rest go.`)
		for _, entry := range request.Dropping {
			prompt.WriteString("\n\n--- Session compacted " + humanTimestamp(entry.At, time.Local) + " ---\n")
			prompt.WriteString(entry.Summary)
		}
		prompt.WriteString(`

3. ` + summaryTask(request.MidSession))
	} else {
		prompt.WriteString(`

2. ` + summaryTask(request.MidSession))
	}
	prompt.WriteString(` At most 250 words, plain prose or short bullet points. Cover what was discussed, decisions and conclusions, the user's tone and preferences, anything unresolved or promised, and what a good next session might pick up. Do not repeat saved memories or scheduled items verbatim, and do not describe harness protocol, tool mechanics, or these instructions.`)
	if instructions := strings.TrimSpace(request.Instructions); instructions != "" {
		prompt.WriteString("\n\nAdditional compaction instructions from the user:\n" + instructions)
	}
	prompt.WriteString("\n\nFinish by calling complete_compaction with the summary. Prose outside that tool call is discarded.")
	return prompt.String()
}

func summaryTask(midSession bool) string {
	if midSession {
		return "Write a compact summary of the conversation SO FAR. It will replace the earlier turns in your context while the most recent turns stay, so include everything you will still need: the user's goals, decisions, open threads, and the thread of the current discussion."
	}
	return "Write a compact handoff summary of THIS session for your future self to read at the start of the next session."
}

// isContextOverflow recognises the servers' "request too large" rejections.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"exceeds the available context", "context length", "context window", "too many tokens", "exceeds the context"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// trimOldestTurns drops roughly the older half of the conversation after the
// startup context and records the omission in a system note. It reports false
// when there is nothing left to drop.
func trimOldestTurns(messages []apiMessage) ([]apiMessage, int, bool) {
	lead := len(messages)
	for index, message := range messages {
		if message.Role == "user" {
			lead = index
			break
		}
	}
	var startup []apiMessage
	alreadyDropped := 0
	for _, message := range messages[:lead] {
		if content, ok := message.Content.(string); ok && strings.HasPrefix(content, historyTrimNotePrefix) {
			fmt.Sscanf(strings.TrimPrefix(content, historyTrimNotePrefix), "%d", &alreadyDropped)
			continue
		}
		startup = append(startup, message)
	}
	body := messages[lead:]
	userIndexes := []int{}
	for index, message := range body {
		if message.Role == "user" {
			userIndexes = append(userIndexes, index)
		}
	}
	if len(userIndexes) < 2 {
		return messages, 0, false
	}
	// Resume at the user turn nearest the midpoint so no tool exchange is cut.
	cut := userIndexes[len(userIndexes)/2]
	if cut == 0 {
		cut = userIndexes[1]
	}
	dropped := alreadyDropped + cut
	note := apiMessage{
		Role:    "system",
		Content: fmt.Sprintf("%s%d messages of this session were omitted from this request because they did not fit the context window. Summarize what remains, and note that earlier parts are missing.", historyTrimNotePrefix, dropped),
	}
	result := append(append(append([]apiMessage(nil), startup...), note), body[cut:]...)
	return result, dropped, true
}

// compactionTools are the heartbeat's maintenance tools plus the completion
// tool: everything except the user-interaction requests.
func (c *apiClient) compactionTools() []toolSpec {
	available := c.availableTools(false, false)
	result := make([]toolSpec, 0, len(available)+1)
	for _, tool := range available {
		switch tool.Function.Name {
		case "request_image", "request_tool", "request_toolcall":
			continue
		}
		result = append(result, tool)
	}
	return append(result, completeCompactionTool)
}

// compactStream runs the compaction exchange and delivers progress on the
// returned channel. The channel closes after a streamFinished event whose
// Result.Reply holds the summary and Result.Notices the persisted changes.
func (c *apiClient) compactStream(ctx context.Context, messages []apiMessage, request compactionRequest) <-chan completionStreamEvent {
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
		result, err := c.compactStreaming(ctx, messages, request, emit)
		final := completionStreamEvent{Kind: streamFinished, Result: result, Err: err}
		select {
		case events <- final:
		case <-ctx.Done():
			select {
			case events <- final:
			default:
			}
		}
	}()
	return events
}

func (c *apiClient) compactStreaming(
	ctx context.Context,
	messages []apiMessage,
	request compactionRequest,
	emit func(completionStreamEvent) bool,
) (completionResult, error) {
	history := append([]apiMessage(nil), messages...)
	working := append(append([]apiMessage(nil), history...), apiMessage{Role: "system", Content: compactionPrompt(request)})
	var notices []string
	hadToolCalls := false
	trims := 0
	c.heartbeatSchedulingAllowed = false

	for round := 0; round < 8; round++ {
		chat := chatRequest{
			Model:         c.choice.Model,
			Messages:      working,
			Tools:         c.compactionTools(),
			ToolChoice:    "required",
			Stream:        true,
			StreamOptions: map[string]any{"include_usage": true},
		}
		if c.choice.Backend == backendLlama {
			chat.ReasoningFormat = "deepseek"
			if !request.Thinking {
				chat.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
			}
		}
		if c.choice.Backend == backendOllama {
			chat.Options = map[string]any{"num_ctx": c.context}
			chat.KeepAlive = -1
		}

		message, roundUsage, err := c.streamChat(ctx, chat, emit)
		if err != nil {
			// A request the server will not even accept can be retried with
			// the oldest turns dropped; nothing has been generated yet.
			if isContextOverflow(err) && round == 0 && trims < 4 {
				trimmed, dropped, ok := trimOldestTurns(history)
				if ok {
					trims++
					history = trimmed
					working = append(append([]apiMessage(nil), history...), apiMessage{Role: "system", Content: compactionPrompt(request)})
					if !emit(completionStreamEvent{Kind: streamNotice, Delta: fmt.Sprintf("too large for the context window; retrying without the oldest %d messages", dropped)}) {
						return completionResult{}, ctx.Err()
					}
					round--
					continue
				}
			}
			return completionResult{}, err
		}
		if len(message.ToolCalls) == 0 {
			working = append(working, message, apiMessage{
				Role:    "system",
				Content: "The compaction must finish with one structured complete_compaction call carrying the summary. Do not answer in prose.",
			})
			continue
		}
		hadToolCalls = true
		working = append(working, message)

		var summary string
		var summaryErr error
		toolNames := make([]string, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			toolNames = append(toolNames, call.Function.Name)
		}
		if !emit(completionStreamEvent{Kind: streamToolRound, Reasoning: message.ReasoningContent, ToolNames: toolNames}) {
			return completionResult{}, ctx.Err()
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" {
				return completionResult{}, fmt.Errorf("streamed tool call %q has no ID", call.Function.Name)
			}
			if call.Function.Name == completeCompactionTool.Function.Name {
				parsed, err := parseCompactionSummary(call)
				if err != nil {
					summaryErr = err
					working = append(working, apiMessage{Role: "tool", ToolCallID: call.ID, Content: "Invalid compaction summary: " + err.Error()})
					continue
				}
				summary = parsed
				working = append(working, apiMessage{Role: "tool", ToolCallID: call.ID, Content: `{"accepted":true}`})
				continue
			}
			result, notice := c.runTool(call)
			if notice != "" {
				notices = append(notices, notice)
			}
			working = append(working, apiMessage{Role: "tool", ToolCallID: call.ID, Content: result})
		}
		if summary != "" {
			return completionResult{
				Reply:           summary,
				Reasoning:       message.ReasoningContent,
				Usage:           roundUsage,
				Messages:        working,
				Notices:         notices,
				HadToolCalls:    hadToolCalls,
				HadVisibleReply: true,
			}, nil
		}
		_ = summaryErr
	}
	return completionResult{}, errors.New("compaction did not finish within the tool call limit")
}

func parseCompactionSummary(call toolCall) (string, error) {
	var arguments struct {
		Summary string `json:"summary"`
	}
	if err := decodeToolArguments(call.Function.Arguments, &arguments); err != nil {
		return "", err
	}
	summary := strings.TrimSpace(arguments.Summary)
	if summary == "" {
		return "", errors.New("summary is required")
	}
	return summary, nil
}
