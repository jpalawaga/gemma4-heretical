package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompleteStreamCollectsReplyReasoningAndUsage(t *testing.T) {
	var received chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"let me "}}]}`,
			`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":", world"}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":42}}`,
			`data: [DONE]`,
		} {
			_, _ = w.Write([]byte(line + "\n\n"))
		}
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "test-model",
		BaseURL: server.URL + "/v1",
		APIKey:  "none",
	}, 8192, 5*time.Second)

	var (
		kinds     []completionStreamEventKind
		reasoning strings.Builder
		content   strings.Builder
		final     completionStreamEvent
	)
	for event := range client.completeStream(context.Background(), []apiMessage{{Role: "user", Content: "hi"}}) {
		kinds = append(kinds, event.Kind)
		switch event.Kind {
		case streamReasoningDelta:
			reasoning.WriteString(event.Delta)
		case streamContentDelta:
			content.WriteString(event.Delta)
		case streamFinished:
			final = event
		}
	}

	if final.Err != nil {
		t.Fatalf("unexpected error: %v", final.Err)
	}
	if kinds[0] != streamConnected || kinds[len(kinds)-1] != streamFinished {
		t.Fatalf("unexpected event order: %v", kinds)
	}
	if got := final.Result.Reply; got != "Hello, world" {
		t.Errorf("reply = %q", got)
	}
	if got := final.Result.Reasoning; got != "let me think" {
		t.Errorf("reasoning = %q", got)
	}
	if content.String() != final.Result.Reply || reasoning.String() != final.Result.Reasoning {
		t.Errorf("streamed deltas %q/%q differ from result", content.String(), reasoning.String())
	}
	if final.Result.Usage.PromptTokens != 42 {
		t.Errorf("prompt tokens = %d", final.Result.Usage.PromptTokens)
	}
	if received.Model != "test-model" || !received.Stream || received.ReasoningFormat != "deepseek" {
		t.Errorf("request not shaped for llama.cpp: %+v", received)
	}
	if received.Options != nil {
		t.Errorf("llama.cpp request should not carry Ollama options: %+v", received.Options)
	}
}

func TestCompleteStreamReportsHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{Backend: backendOllama, BaseURL: server.URL + "/v1", APIKey: "ollama"}, 4096, 5*time.Second)
	var final completionStreamEvent
	for event := range client.completeStream(context.Background(), nil) {
		final = event
	}
	if final.Kind != streamFinished || final.Err == nil {
		t.Fatalf("expected a finished event with an error, got %+v", final)
	}
	if !strings.Contains(final.Err.Error(), "model not loaded") {
		t.Errorf("error should include the server message: %v", final.Err)
	}
}

func TestClearCommandKeepsOnlySystemMessages(t *testing.T) {
	m := newAppModel(appConfig{Context: 1024})
	m.width, m.height = 100, 40
	m.messages = []apiMessage{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	m.transcript = []transcriptMessage{{Role: "user", Content: "hi"}}
	if !m.handleCommand("/clear") {
		t.Fatal("/clear was not handled")
	}
	if len(m.messages) != 1 || m.messages[0].Role != "system" {
		t.Errorf("messages after /clear = %+v", m.messages)
	}
	if len(m.transcript) != 0 {
		t.Errorf("transcript after /clear = %+v", m.transcript)
	}
}

func TestFailedResponseDropsUnansweredUserTurn(t *testing.T) {
	m := newAppModel(appConfig{Context: 1024})
	m.width, m.height = 100, 40
	m.messages = []apiMessage{{Role: "user", Content: "hi"}}
	m.busy = true
	updated, _ := m.Update(responseMsg{err: context.DeadlineExceeded})
	result := updated.(appModel)
	if result.busy {
		t.Error("model should no longer be busy")
	}
	if len(result.messages) != 0 {
		t.Errorf("unanswered user turn should be dropped, got %+v", result.messages)
	}
	if len(result.transcript) != 1 || result.transcript[0].Role != "error" {
		t.Errorf("expected one error transcript entry, got %+v", result.transcript)
	}
}
