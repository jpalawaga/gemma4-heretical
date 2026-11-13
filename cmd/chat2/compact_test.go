package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestCompactedConversationHistoryRetainsRecentSessions(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	if entries, err := store.CompactedConversations(); err != nil || len(entries) != 0 {
		t.Fatalf("expected empty history, got %#v err=%v", entries, err)
	}
	if _, err := store.AppendCompactedConversation("", time.Now(), 5); err == nil {
		t.Fatal("empty summaries must be rejected")
	}
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local)
	for day := 0; day < 5; day++ {
		dropped, err := store.AppendCompactedConversation(fmt.Sprintf("session %d summary", day), base.AddDate(0, 0, day), 5)
		if err != nil || len(dropped) != 0 {
			t.Fatalf("append %d: dropped=%#v err=%v", day, dropped, err)
		}
	}
	overflow, err := store.CompactionOverflow(5)
	if err != nil || len(overflow) != 1 || overflow[0].Summary != "session 0 summary" {
		t.Fatalf("overflow preview wrong: %#v %v", overflow, err)
	}
	dropped, err := store.AppendCompactedConversation("session 5 summary", base.AddDate(0, 0, 5), 5)
	if err != nil || len(dropped) != 1 || dropped[0].Summary != "session 0 summary" || !dropped[0].At.Equal(base) {
		t.Fatalf("sixth append should drop the oldest: %#v %v", dropped, err)
	}
	entries, err := store.CompactedConversations()
	if err != nil || len(entries) != 5 || entries[0].Summary != "session 1 summary" || entries[4].Summary != "session 5 summary" {
		t.Fatalf("history after trim: %#v %v", entries, err)
	}
	raw, _ := os.ReadFile(filepath.Join(store.dir, compactedConversationFile))
	if strings.Count(string(raw), compactedSessionHeader) != 5 || !strings.Contains(string(raw), compactedSessionHeader+base.AddDate(0, 0, 5).Format(time.RFC3339)) {
		t.Fatalf("unexpected file layout:\n%s", raw)
	}
	latest, ok, err := store.CompactedConversation()
	if err != nil || !ok || latest.Summary != "session 5 summary" {
		t.Fatalf("latest = %#v ok=%v err=%v", latest, ok, err)
	}

	if err := store.AppendRecentMessage("user", "previous question"); err != nil {
		t.Fatal(err)
	}
	systemFile := filepath.Join(t.TempDir(), "startup.md")
	if err := os.WriteFile(systemFile, []byte("You are a careful local companion."), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, _, _, err := store.InitialSystemMessages(systemFile)
	if err != nil {
		t.Fatal(err)
	}
	summaryIndex, recentIndex := -1, -1
	for index, message := range initial {
		content, _ := message.Content.(string)
		if strings.Contains(content, "last 5 sessions, oldest first") && strings.Contains(content, "session 1 summary") && strings.Contains(content, "session 5 summary") {
			summaryIndex = index
			if strings.Index(content, "session 1 summary") > strings.Index(content, "session 5 summary") {
				t.Error("sessions should be listed oldest first")
			}
			if strings.Contains(content, "session 0 summary") {
				t.Error("dropped session leaked into startup context")
			}
		}
		if strings.Contains(content, "previous question") {
			recentIndex = index
		}
	}
	if summaryIndex != 1 || recentIndex != 2 {
		t.Fatalf("startup ordering wrong: summary=%d recent=%d in %#v", summaryIndex, recentIndex, initial)
	}
}

func TestLegacyCompactedFileWithoutHeaderStillLoads(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, compactedConversationFile), []byte("plain summary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compacted, ok, err := store.CompactedConversation()
	if err != nil || !ok || compacted.Summary != "plain summary" || !compacted.At.IsZero() {
		t.Fatalf("legacy file was not loaded: ok=%v err=%v %#v", ok, err, compacted)
	}
	at := time.Date(2026, 10, 15, 22, 30, 0, 0, time.Local)
	if err := store.SaveCompactedConversation("single entry", at); err != nil {
		t.Fatal(err)
	}
	entries := parseCompactedConversations(compactedConversationHeader + at.Format(time.RFC3339) + "\n\nold format\n")
	if len(entries) != 1 || entries[0].Summary != "old format" || !entries[0].At.Equal(at) {
		t.Fatalf("old header format not parsed: %#v", entries)
	}
}

func TestControlCWithoutRepliesQuitsImmediately(t *testing.T) {
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.phase = phaseChatting
	m.client = newAPIClient(modelChoice{Backend: backendLlama, BaseURL: "http://127.0.0.1:1", APIKey: "none"}, 16384, m.memory, time.Second)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if updated.(appModel).phase != phaseChatting {
		t.Fatalf("empty session should not enter compaction: %v", updated.(appModel).phase)
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("expected tea.Quit when nothing happened this session")
	}
}

func TestControlCCompactsConversationBeforeExit(t *testing.T) {
	var requests []chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		call := func(id, name, arguments string) map[string]any {
			return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"role":       "assistant",
				"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}},
			}}}}
		}
		usage := map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 12}}
		if len(requests) == 1 {
			writeSSE(w, call("m-1", "save_memory", `{"content":"The user is shopping for a wider-field eyepiece.","category":"general"}`), usage)
			return
		}
		writeSSE(w, call("c-1", "complete_compaction", `{"summary":"We talked about telescopes."}`), usage)
	}))
	defer server.Close()

	store := testMemoryStore(t)
	instructions := filepath.Join(t.TempDir(), "compact.md")
	if err := os.WriteFile(instructions, []byte("Always note the user's mood."), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	for day := 0; day < 5; day++ {
		if _, err := store.AppendCompactedConversation(fmt.Sprintf("older session %d", day), base.AddDate(0, 0, day), 5); err != nil {
			t.Fatal(err)
		}
	}
	cfg := appConfig{RequestTimeout: 5 * time.Second, CompactionHistory: 5, CompactionInstructionsFile: instructions}
	m := newAppModel(cfg, store)
	m.phase = phaseChatting
	m.width, m.height = 80, 30
	m.resize()
	m.client = newAPIClient(modelChoice{Backend: backendLlama, Model: "test", BaseURL: server.URL, APIKey: "none"}, 16384, store, 5*time.Second)
	m.client.exposeAllTools = true
	m.messages = []apiMessage{
		{Role: "system", Content: "startup"},
		{Role: "user", Content: "tell me about telescopes"},
		{Role: "assistant", Content: "Refractors and reflectors differ in..."},
	}
	m.sessionReplies = 1

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(appModel)
	if m.phase != phaseCompacting || !m.busy || cmd == nil {
		t.Fatalf("Ctrl+C did not start compaction: phase=%v busy=%v cmd=%v", m.phase, m.busy, cmd != nil)
	}
	if view := m.View().Content; !strings.Contains(view, "Compacting conversation") || !strings.Contains(view, "Ctrl+C again") || !strings.Contains(view, "1 older session summary will be archived") {
		t.Fatalf("compaction panel is wrong: %s", view)
	}

	var quit bool
	msg := cmd()
	for !quit {
		var next tea.Cmd
		updated, next = m.Update(msg)
		m = updated.(appModel)
		if next == nil {
			t.Fatal("compaction stalled without a follow-up command")
		}
		msg = next()
		if _, ok := msg.(tea.QuitMsg); ok {
			quit = true
		}
	}

	if !strings.Contains(m.exitNotice, "Conversation compacted to") || !strings.Contains(m.exitNotice, "1 older summary removed") || !strings.Contains(m.exitNotice, "memory saved") {
		t.Fatalf("unexpected exit notice: %q", m.exitNotice)
	}
	if len(m.compactLog) < 3 || !strings.Contains(m.compactLog[1], "save_memory") || !strings.Contains(m.compactLog[2], "complete_compaction") {
		t.Fatalf("progress log wrong: %#v", m.compactLog)
	}
	entries, err := store.CompactedConversations()
	if err != nil || len(entries) != 5 || entries[4].Summary != "We talked about telescopes." || entries[0].Summary != "older session 1" {
		t.Fatalf("history after compaction: %#v %v", entries, err)
	}
	if memories, _ := store.Load(); len(memories) != 1 || !strings.Contains(memories[0].Content, "eyepiece") {
		t.Fatalf("the compaction agent's memory save was not applied: %#v", memories)
	}

	if len(requests) != 2 {
		t.Fatalf("expected two rounds, got %d", len(requests))
	}
	first := requests[0]
	if first.ToolChoice != "required" {
		t.Fatalf("compaction must require a tool call, got %#v", first.ToolChoice)
	}
	for _, name := range []string{"save_memory", "update_memory", "schedule_item", "complete_compaction"} {
		if !hasTool(first.Tools, name) {
			t.Errorf("compaction tools missing %s", name)
		}
	}
	for _, name := range []string{"request_image", "request_tool", "request_toolcall", "complete_heartbeat"} {
		if hasTool(first.Tools, name) {
			t.Errorf("compaction tools should not offer %s", name)
		}
	}
	if enabled, ok := first.ChatTemplateKwargs["enable_thinking"].(bool); !ok || enabled {
		t.Fatalf("thinking should be disabled for compaction by default: %#v", first.ChatTemplateKwargs)
	}
	last := first.Messages[len(first.Messages)-1]
	content, _ := last.Content.(string)
	if last.Role != "system" || !strings.Contains(content, "HARNESS REQUEST: compact_conversation") {
		t.Fatalf("compaction request was not appended as a system message: %#v", last)
	}
	for _, expected := range []string{"older session 0", "about to be deleted", "Always note the user's mood.", "complete_compaction"} {
		if !strings.Contains(content, expected) {
			t.Errorf("compaction prompt omitted %q", expected)
		}
	}
	if strings.Contains(content, "older session 1") {
		t.Error("retained summaries should not be offered for archiving")
	}
	if encoded, _ := json.Marshal(first.Messages); !strings.Contains(string(encoded), "tell me about telescopes") {
		t.Fatalf("conversation history was not sent for compaction: %s", encoded)
	}
	second := requests[1]
	if encoded, _ := json.Marshal(second.Messages); !strings.Contains(string(encoded), "Saved in general") {
		t.Fatalf("second round should carry the tool result: %s", encoded)
	}
}

func TestCompactionThinkingSettingKeepsReasoningOn(t *testing.T) {
	t.Parallel()
	prompt := compactionPrompt(compactionRequest{})
	if strings.Contains(prompt, "about to be deleted") || !strings.Contains(prompt, "2. Write a compact handoff summary") {
		t.Fatalf("prompt without overflow should skip the archive step: %s", prompt)
	}
	var sawKwargs map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		sawKwargs = request.ChatTemplateKwargs
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"role":       "assistant",
			"tool_calls": []any{map[string]any{"index": 0, "id": "c-1", "type": "function", "function": map[string]any{"name": "complete_compaction", "arguments": `{"summary":"ok"}`}}},
		}}}})
	}))
	defer server.Close()
	client := newAPIClient(modelChoice{Backend: backendLlama, Model: "test", BaseURL: server.URL, APIKey: "none"}, 16384, testMemoryStore(t), 5*time.Second)
	var final completionStreamEvent
	for event := range client.compactStream(context.Background(), []apiMessage{{Role: "user", Content: "hi"}}, compactionRequest{Thinking: true}) {
		final = event
	}
	if final.Err != nil || final.Result.Reply != "ok" {
		t.Fatalf("compaction failed: %+v", final)
	}
	if sawKwargs != nil {
		t.Fatalf("thinking on should not send template kwargs: %#v", sawKwargs)
	}
}

func TestSecondControlCSkipsCompaction(t *testing.T) {
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
	if err := store.SaveCompactedConversation("older summary", time.Now()); err != nil {
		t.Fatal(err)
	}
	m := newAppModel(appConfig{RequestTimeout: 5 * time.Second}, store)
	m.phase = phaseChatting
	m.width, m.height = 80, 30
	m.resize()
	m.client = newAPIClient(modelChoice{Backend: backendLlama, Model: "test", BaseURL: server.URL, APIKey: "none"}, 16384, store, 5*time.Second)
	m.messages = []apiMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
	m.sessionReplies = 1

	updated, startCmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(appModel)
	if m.phase != phaseCompacting {
		t.Fatalf("expected compaction phase, got %v", m.phase)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := make(chan tea.Msg, 1)
	go func() { started <- startCmd() }()

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(appModel)
	if cmd == nil {
		t.Fatal("second Ctrl+C must quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("second Ctrl+C must return tea.Quit")
	}
	if !strings.Contains(m.exitNotice, "skipped") {
		t.Fatalf("unexpected exit notice: %q", m.exitNotice)
	}
	// Cancelling must unblock the in-flight request promptly.
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("cancelled compaction request did not return")
	}
	compacted, ok, err := store.CompactedConversation()
	if err != nil || !ok || compacted.Summary != "older summary" {
		t.Fatalf("skipping must leave the previous summary intact: %#v", compacted)
	}
}
