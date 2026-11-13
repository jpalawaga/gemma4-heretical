package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestTrimOldestTurnsDropsHalfAndKeepsStartup(t *testing.T) {
	t.Parallel()
	messages := []apiMessage{
		{Role: "system", Content: "startup"},
		{Role: "system", Content: "memories"},
		{Role: "user", Content: "q1"}, {Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"}, {Role: "assistant", Content: "", ToolCalls: []toolCall{{ID: "t", Function: functionCall{Name: "get_current_time"}}}}, {Role: "tool", ToolCallID: "t", Content: "noon"}, {Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"}, {Role: "assistant", Content: "a3"},
		{Role: "user", Content: "q4"}, {Role: "assistant", Content: "a4"},
	}
	trimmed, dropped, ok := trimOldestTurns(messages)
	if !ok || dropped == 0 {
		t.Fatalf("trim did nothing: ok=%v dropped=%d", ok, dropped)
	}
	if trimmed[0].Content != "startup" || trimmed[1].Content != "memories" {
		t.Fatalf("startup context must be kept: %#v", trimmed[:2])
	}
	note, _ := trimmed[2].Content.(string)
	if trimmed[2].Role != "system" || !strings.HasPrefix(note, historyTrimNotePrefix) {
		t.Fatalf("trim note missing: %#v", trimmed[2])
	}
	if trimmed[3].Role != "user" {
		t.Fatalf("kept history must resume at a user turn, got %#v", trimmed[3])
	}
	encoded, _ := json.Marshal(trimmed)
	if strings.Contains(string(encoded), "\"q1\"") || !strings.Contains(string(encoded), "\"q4\"") {
		t.Fatalf("wrong turns dropped: %s", encoded)
	}
	// A second trim accumulates the count rather than stacking notes.
	again, droppedAgain, ok := trimOldestTurns(trimmed)
	if !ok || droppedAgain <= dropped {
		t.Fatalf("second trim should drop more: ok=%v %d -> %d", ok, dropped, droppedAgain)
	}
	notes := 0
	for _, message := range again {
		if content, _ := message.Content.(string); strings.HasPrefix(content, historyTrimNotePrefix) {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("expected one cumulative note, got %d", notes)
	}
	if _, _, ok := trimOldestTurns([]apiMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "only"}}); ok {
		t.Fatal("a single turn cannot be trimmed")
	}
}

func TestCompactionRetriesWithTrimmedHistoryOnContextOverflow(t *testing.T) {
	var requests []chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests = append(requests, request)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"request (33622 tokens) exceeds the available context size (32768 tokens), try increasing it","type":"invalid_request_error"}}`))
			return
		}
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"role":       "assistant",
			"tool_calls": []any{map[string]any{"index": 0, "id": "c-1", "type": "function", "function": map[string]any{"name": "complete_compaction", "arguments": `{"summary":"Condensed."}`}}},
		}}}})
	}))
	defer server.Close()

	store := testMemoryStore(t)
	m := newAppModel(appConfig{RequestTimeout: 5 * time.Second, CompactionHistory: 5}, store)
	m.phase = phaseChatting
	m.width, m.height = 80, 30
	m.resize()
	m.client = newAPIClient(modelChoice{Backend: backendLlama, Model: "test", BaseURL: server.URL, APIKey: "none"}, 32768, store, 5*time.Second)
	m.messages = []apiMessage{{Role: "system", Content: "startup"}}
	for turn := 1; turn <= 6; turn++ {
		m.messages = append(m.messages, apiMessage{Role: "user", Content: "question " + string(rune('0'+turn))}, apiMessage{Role: "assistant", Content: "answer"})
	}
	m.sessionReplies = 6

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(appModel)
	msg := cmd()
	for {
		var next tea.Cmd
		updated, next = m.Update(msg)
		m = updated.(appModel)
		if next == nil {
			t.Fatal("compaction stalled")
		}
		msg = next()
		if _, ok := msg.(tea.QuitMsg); ok {
			break
		}
	}
	if len(requests) != 2 {
		t.Fatalf("expected a retry, got %d requests", len(requests))
	}
	if len(requests[1].Messages) >= len(requests[0].Messages) {
		t.Fatalf("retry should send fewer messages: %d -> %d", len(requests[0].Messages), len(requests[1].Messages))
	}
	second, _ := json.Marshal(requests[1].Messages)
	if !strings.Contains(string(second), historyTrimNotePrefix) || strings.Contains(string(second), "question 1") || !strings.Contains(string(second), "question 6") {
		t.Fatalf("retry history wrong: %s", second)
	}
	if !strings.Contains(m.exitNotice, "Conversation compacted to") {
		t.Fatalf("compaction should have succeeded after the retry: %q", m.exitNotice)
	}
	joined := strings.Join(m.compactLog, "\n")
	if !strings.Contains(joined, "retrying without the oldest") {
		t.Fatalf("progress log should mention the trim: %#v", m.compactLog)
	}
	if latest, ok, _ := store.CompactedConversation(); !ok || latest.Summary != "Condensed." {
		t.Fatalf("summary not stored: %#v", latest)
	}
}

func TestSlashCompactReplacesHistoryAndKeepsChatting(t *testing.T) {
	var saw chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&saw)
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"role":       "assistant",
			"tool_calls": []any{map[string]any{"index": 0, "id": "c-1", "type": "function", "function": map[string]any{"name": "complete_compaction", "arguments": `{"summary":"We planned the telescope purchase."}`}}},
		}}}})
	}))
	defer server.Close()

	store := testMemoryStore(t)
	if _, err := store.AppendCompactedConversation("older session", time.Now().Add(-24*time.Hour), 5); err != nil {
		t.Fatal(err)
	}
	m := newAppModel(appConfig{RequestTimeout: 5 * time.Second, CompactionHistory: 5, Context: 32768}, store)
	m.phase = phaseChatting
	m.width, m.height = 80, 30
	m.resize()
	m.client = newAPIClient(modelChoice{Backend: backendLlama, Model: "test", BaseURL: server.URL, APIKey: "none"}, 32768, store, 5*time.Second)
	m.messages = []apiMessage{{Role: "system", Content: "startup"}, {Role: "system", Content: "[HARNESS CONFIGURATION] labels"}}
	m.startupMessageCount = 2
	for turn := 1; turn <= 4; turn++ {
		m.messages = append(m.messages, apiMessage{Role: "user", Content: "question " + string(rune('0'+turn))}, apiMessage{Role: "assistant", Content: "answer " + string(rune('0'+turn))})
	}
	m.sessionReplies = 4

	handled, cmd := m.handleCommand("/compact")
	if !handled || cmd == nil || m.phase != phaseCompacting || !m.compactMidSession {
		t.Fatalf("/compact did not start: handled=%v phase=%v mid=%v", handled, m.phase, m.compactMidSession)
	}
	if !strings.Contains(m.View().Content, "The chat continues afterwards") {
		t.Fatalf("mid-session panel wording missing: %s", m.View().Content)
	}
	msg := cmd()
	for m.phase == phaseCompacting {
		var next tea.Cmd
		updated, next := m.Update(msg)
		m = updated.(appModel)
		if m.phase != phaseCompacting {
			break
		}
		if next == nil {
			t.Fatal("compaction stalled")
		}
		msg = next()
	}
	if m.phase != phaseChatting || m.busy {
		t.Fatalf("should return to chat: phase=%v busy=%v", m.phase, m.busy)
	}
	prompt, _ := saw.Messages[len(saw.Messages)-1].Content.(string)
	if !strings.Contains(prompt, "conversation is continuing") || strings.Contains(prompt, "about to be deleted") {
		t.Fatalf("mid-session prompt wrong: %s", prompt)
	}
	// Startup context, the summary note, then the last two user turns.
	if len(m.messages) != 2+1+4 {
		t.Fatalf("compacted history has %d messages: %#v", len(m.messages), m.messages)
	}
	note, _ := m.messages[2].Content.(string)
	if m.messages[0].Content != "startup" || m.messages[2].Role != "system" || !strings.Contains(note, "We planned the telescope purchase.") || !strings.Contains(note, "compacted at") {
		t.Fatalf("summary note wrong: %#v", m.messages[:3])
	}
	if m.messages[3].Content != "question 3" || m.messages[6].Content != "answer 4" {
		t.Fatalf("recent turns wrong: %#v", m.messages[3:])
	}
	last := m.transcript[len(m.transcript)-2]
	if last.Role != "notice" || len(last.Chips) == 0 || last.Chips[0].Label != "Conversation Compacted" {
		t.Fatalf("transcript notice missing: %#v", last)
	}
	if entries, _ := store.CompactedConversations(); len(entries) != 1 || entries[0].Summary != "older session" {
		t.Fatalf("mid-session compaction must not touch the history file: %#v", entries)
	}
	if m.promptTokens != 0 {
		t.Fatalf("prompt token meter should reset until the next reply: %d", m.promptTokens)
	}
}

func TestSlashCompactCancelReturnsToChat(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(block)
	store := testMemoryStore(t)
	m := newAppModel(appConfig{RequestTimeout: 5 * time.Second, CompactionHistory: 5}, store)
	m.phase = phaseChatting
	m.width, m.height = 80, 30
	m.resize()
	m.client = newAPIClient(modelChoice{Backend: backendLlama, Model: "test", BaseURL: server.URL, APIKey: "none"}, 32768, store, 5*time.Second)
	m.messages = []apiMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
	m.sessionReplies = 1
	_, cmd := m.handleCommand("/compact")
	go cmd()
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(appModel)
	if m.phase != phaseChatting || m.busy || len(m.messages) != 2 || !strings.Contains(m.status, "cancelled") {
		t.Fatalf("cancel should return to chat unchanged: phase=%v busy=%v status=%q", m.phase, m.busy, m.status)
	}
}

func TestContextFullWarningAfterReply(t *testing.T) {
	m := newAppModel(appConfig{Context: 1000}, testMemoryStore(t))
	m.phase = phaseChatting
	m.width, m.height = 80, 30
	m.resize()
	m.busy = true
	updated, _ := m.Update(responseMsg{result: completionResult{Reply: "ok", Usage: usage{PromptTokens: 900}, Messages: []apiMessage{{Role: "user", Content: "q"}}}})
	m = updated.(appModel)
	if !strings.Contains(m.status, "context 90% full") || !strings.Contains(m.status, "/compact") {
		t.Fatalf("expected a context warning, got %q", m.status)
	}
}
