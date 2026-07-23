package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestSplitArgs(t *testing.T) {
	t.Parallel()
	got, err := splitArgs(`one.png "two words.jpg" 'three words.gif' escaped\ path.tga`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"one.png", "two words.jpg", "three words.gif", "escaped path.tga"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestImageParts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pixel.png")
	content := []byte("\x89PNG\r\n\x1a\nfake")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	parts, err := imageParts("what is this?", []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Text != "what is this?" {
		t.Fatalf("unexpected parts: %#v", parts)
	}
	want := base64.StdEncoding.EncodeToString(content)
	if parts[1].ImageURL == nil || !strings.HasSuffix(parts[1].ImageURL.URL, want) {
		t.Fatalf("image was not encoded as a data URL: %#v", parts[1])
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	if _, err := store.Save("prefers dark themes", []string{"preferences", "ui"}); err != nil {
		t.Fatal(err)
	}
	if result := store.Search("dark"); !strings.Contains(result, "prefers dark themes") {
		t.Fatalf("search missed memory: %q", result)
	}
	if result := store.SearchTag("UI"); !strings.Contains(result, "prefers dark themes") {
		t.Fatalf("tag search missed memory: %q", result)
	}
	removed, err := store.Forget("dark")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d entries, want 1", removed)
	}
}

func TestNativeScheduleStoreAndSnapshot(t *testing.T) {
	location := time.FixedZone("MDT", -6*60*60)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, location)
	store := newScheduleStore(t.TempDir())

	due, err := store.Add(now.Add(-time.Hour), "past-due task", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	upcoming, err := store.Add(now.Add(2*time.Hour), "nearby task", now)
	if err != nil {
		t.Fatal(err)
	}
	later, err := store.Add(now.Add(72*time.Hour), "later task", now)
	if err != nil {
		t.Fatal(err)
	}
	historical, err := store.Add(now.Add(-3*time.Hour), "finished task", now.Add(-4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(historical.ID, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.Snapshot(now, 48*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Due) != 1 || snapshot.Due[0].ID != due.ID {
		t.Fatalf("due snapshot = %#v", snapshot.Due)
	}
	if len(snapshot.Upcoming) != 1 || snapshot.Upcoming[0].ID != upcoming.ID {
		t.Fatalf("upcoming snapshot = %#v", snapshot.Upcoming)
	}
	if len(snapshot.RecentCompleted) != 1 || snapshot.RecentCompleted[0].ID != historical.ID {
		t.Fatalf("recent history = %#v", snapshot.RecentCompleted)
	}
	if len(snapshot.Later) != 1 || snapshot.Later[0].ID != later.ID {
		t.Fatalf("later snapshot = %#v", snapshot.Later)
	}
	for _, group := range [][]scheduleEntry{snapshot.Due, snapshot.Upcoming, snapshot.RecentCompleted} {
		for _, entry := range group {
			if entry.ID == later.ID {
				t.Fatalf("later item leaked into current metadata: %#v", snapshot)
			}
		}
	}
	becameUpcoming, err := store.Snapshot(now.Add(25*time.Hour), 48*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !scheduleEntriesContain(becameUpcoming.Upcoming, later.ID) {
		t.Fatalf("item was not injected when it entered the upcoming horizon: %#v", becameUpcoming)
	}
	becameDue, err := store.Snapshot(now.Add(73*time.Hour), 48*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !scheduleEntriesContain(becameDue.Due, later.ID) {
		t.Fatalf("item was not injected when it became due: %#v", becameDue)
	}

	reloaded := newScheduleStore(filepath.Dir(store.path))
	entries, err := reloaded.List()
	if err != nil {
		t.Fatal(err)
	}
	completedPersisted := false
	for _, entry := range entries {
		if entry.ID == historical.ID && entry.CompletedAt != nil {
			completedPersisted = true
		}
	}
	if len(entries) != 4 || !completedPersisted {
		t.Fatalf("completed history was not persisted: %#v", entries)
	}
	next, err := reloaded.Add(now.Add(96*time.Hour), "next task", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != 5 {
		t.Fatalf("next schedule ID = %d, want 5", next.ID)
	}
}

func scheduleEntriesContain(entries []scheduleEntry, id int) bool {
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}

func TestScheduleStoreReadsLegacyScheduleRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedule.jsonl")
	if err := os.WriteFile(path, []byte(`{"id":7,"time":"2026-07-21T10:00:00-06:00","event":"legacy item"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newScheduleStore(dir)
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != 7 || entries[0].Event != "legacy item" {
		t.Fatalf("legacy schedule row was not loaded: %#v", entries)
	}
	next, err := store.Add(time.Date(2026, 7, 22, 10, 0, 0, 0, entries[0].Time.Location()), "native item", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != 8 {
		t.Fatalf("native item ID = %d, want 8", next.ID)
	}
}

func TestRecurringScheduleMaterializesThroughOneDayPlusOneLater(t *testing.T) {
	location := time.FixedZone("MDT", -6*60*60)
	now := time.Date(2026, 7, 22, 8, 0, 0, 0, location)
	store := newScheduleStore(t.TempDir())

	first, err := store.AddRecurring(now.Add(time.Hour), "hourly check", "hour", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.SeriesID != first.ID || first.RecurrenceUnit != "hour" || first.RecurrencePeriod != 1 {
		t.Fatalf("recurring seed = %#v", first)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 25 {
		t.Fatalf("materialized %d hourly entries, want 25", len(entries))
	}
	if !entries[len(entries)-1].Time.Equal(now.Add(25 * time.Hour)) {
		t.Fatalf("last materialized occurrence = %s, want %s", entries[len(entries)-1].Time, now.Add(25*time.Hour))
	}
	for index, entry := range entries {
		if entry.ID != index+1 || entry.SeriesID != first.SeriesID || entry.RecurrenceUnit != "hour" || entry.RecurrencePeriod != 1 {
			t.Fatalf("materialized occurrence %d = %#v", index, entry)
		}
	}

	laterNow := now.Add(10 * time.Hour)
	snapshot, err := store.Snapshot(laterNow, defaultScheduleUpcomingWindow, defaultScheduleHistoryWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Due) != maxScheduleDueItems || len(snapshot.Upcoming) != maxScheduleUpcomingItems || len(snapshot.Later) != 1 {
		t.Fatalf("bounded recurring snapshot = %#v", snapshot)
	}
	if !snapshot.Later[0].Time.Equal(now.Add(35 * time.Hour)) {
		t.Fatalf("later recurrence = %s, want %s", snapshot.Later[0].Time, now.Add(35*time.Hour))
	}

	reloaded := newScheduleStore(filepath.Dir(store.path))
	reloadedEntries, err := reloaded.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloadedEntries) != 35 || reloadedEntries[0].RecurrenceUnit != "hour" || reloadedEntries[34].SeriesID != first.SeriesID {
		t.Fatalf("recurring series was not persisted: %#v", reloadedEntries)
	}
}

func TestRecurringScheduleBackfillsOfflineOccurrencesButBoundsDueContext(t *testing.T) {
	location := time.FixedZone("MDT", -6*60*60)
	now := time.Date(2026, 7, 22, 8, 0, 0, 0, location)
	store := newScheduleStore(t.TempDir())
	if _, err := store.AddRecurring(now.Add(time.Hour), "hourly check", "hour", 1, now); err != nil {
		t.Fatal(err)
	}

	returnedAt := now.Add(30 * time.Hour)
	snapshot, err := store.Snapshot(returnedAt, defaultScheduleUpcomingWindow, defaultScheduleHistoryWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Due) != maxScheduleDueItems || !snapshot.Due[0].Time.Equal(now.Add(26*time.Hour)) || !snapshot.Due[4].Time.Equal(now.Add(30*time.Hour)) {
		t.Fatalf("offline due context was not bounded to the latest occurrences: %#v", snapshot.Due)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 55 {
		t.Fatalf("offline recurrence materialized %d entries, want 55", len(entries))
	}
	for index, entry := range entries {
		want := now.Add(time.Duration(index+1) * time.Hour)
		if !entry.Time.Equal(want) {
			t.Fatalf("recurrence gap at entry %d: got %s, want %s", index, entry.Time, want)
		}
	}
}

func TestScheduleToolRecurrenceArguments(t *testing.T) {
	var scheduleTool functionSpec
	for _, tool := range tools {
		if tool.Function.Name == "schedule_item" {
			scheduleTool = tool.Function
			break
		}
	}
	properties, _ := scheduleTool.Parameters["properties"].(map[string]any)
	unitSchema, unitFound := properties["recurrence_unit"].(map[string]any)
	periodSchema, periodFound := properties["recurrence_period"].(map[string]any)
	if !unitFound || !periodFound || !strings.Contains(scheduleTool.Description, "provide both recurrence_unit and recurrence_period") {
		t.Fatalf("schedule tool does not explain recurrence: %#v", scheduleTool)
	}
	if fmt.Sprint(unitSchema["enum"]) != "[hour day week month]" || periodSchema["minimum"] != 1 {
		t.Fatalf("recurrence schema is not discoverable: unit=%#v period=%#v", unitSchema, periodSchema)
	}

	store := testMemoryStore(t)
	client := newAPIClient(modelChoice{Backend: backendLlama}, 16384, store, time.Second)
	when := time.Now().Add(time.Hour).Format(time.RFC3339)

	invalid, _ := client.runTool(toolCall{Function: functionCall{
		Name:      "schedule_item",
		Arguments: fmt.Sprintf(`{"time":%q,"event":"hourly check","recurrence_unit":"hour"}`, when),
	}})
	if !strings.Contains(invalid, "must be provided together") {
		t.Fatalf("incomplete recurrence was accepted: %q", invalid)
	}

	result, notice := client.runTool(toolCall{Function: functionCall{
		Name:      "schedule_item",
		Arguments: fmt.Sprintf(`{"time":%q,"event":"hourly check","recurrence_unit":"hour","recurrence_period":1}`, when),
	}})
	if !strings.Contains(result, `"recurrence_unit":"hour"`) || !strings.Contains(result, `"recurrence_period":1`) || notice != "scheduled recurring series #1" {
		t.Fatalf("recurrence tool result=%q notice=%q", result, notice)
	}
}

func TestScheduleToolsAndResponseEnvelope(t *testing.T) {
	store := testMemoryStore(t)
	client := newAPIClient(modelChoice{Backend: backendLlama}, 16384, store, time.Second)
	result, notice := client.runTool(toolCall{Function: functionCall{
		Name:      "schedule_item",
		Arguments: `{"time":"2026-07-20T13:00:00-06:00","event":"scheduled through the model"}`,
	}})
	if !strings.Contains(result, `"id":1`) || notice != "scheduled item #1" {
		t.Fatalf("schedule tool result=%q notice=%q", result, notice)
	}
	listed, _ := client.runTool(toolCall{Function: functionCall{Name: "list_scheduled_items", Arguments: `{}`}})
	if !strings.Contains(listed, "scheduled through the model") {
		t.Fatalf("list tool omitted scheduled item: %q", listed)
	}
	completed, notice := client.runTool(toolCall{Function: functionCall{
		Name:      "complete_scheduled_item",
		Arguments: `{"id":1}`,
	}})
	if !strings.Contains(completed, `"status":"completed"`) || notice != "completed scheduled item #1" {
		t.Fatalf("completion tool result=%q notice=%q", completed, notice)
	}

	envelopeMemory := testMemoryStore(t)
	location := time.FixedZone("MDT", -6*60*60)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, location)
	m := newAppModel(appConfig{}, envelopeMemory)
	m.lastSent = now.Add(-90 * time.Minute)
	if _, err := m.schedule.Add(now.Add(-time.Hour), "inject due", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.schedule.Add(now.Add(3*time.Hour), "inject upcoming", now); err != nil {
		t.Fatal(err)
	}
	later, err := m.schedule.Add(now.Add(3*24*time.Hour), "do not inject yet", now)
	if err != nil {
		t.Fatal(err)
	}
	history, err := m.schedule.Add(now.Add(-2*time.Hour), "inject recent completion", now.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.schedule.Complete(history.ID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	olderHistory, err := m.schedule.Add(now.Add(-27*time.Hour), "completion outside history window", now.Add(-28*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.schedule.Complete(olderHistory.ID, now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}

	enveloped := m.responseEnvelope("hello", now)
	if !strings.HasSuffix(enveloped, "[/HARNESS METADATA]\nhello") {
		t.Fatalf("user content was not wrapped correctly: %q", enveloped)
	}
	metadataJSON := strings.TrimPrefix(enveloped, "[HARNESS METADATA]\n")
	metadataJSON, _, _ = strings.Cut(metadataJSON, "\n[/HARNESS METADATA]")
	var metadata responseEnvelopeMetadata
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.SinceLastUserMessage != "1h30m0s" || metadata.Schedule == nil {
		t.Fatalf("unexpected envelope metadata: %#v", metadata)
	}
	if metadata.SentAt != "Monday July 20, 12:00 PM MDT" || metadata.Schedule.UpcomingThrough != "Tuesday July 21, 12:00 PM MDT" {
		t.Fatalf("envelope times are not human-readable: %#v", metadata)
	}
	if len(metadata.Schedule.Due) != 1 || len(metadata.Schedule.Upcoming) != 1 || len(metadata.Schedule.RecentlyCompleted) != 1 {
		t.Fatalf("schedule envelope classification is wrong: %#v", metadata.Schedule)
	}
	if metadata.Schedule.Due[0].Time != "Monday July 20, 11:00 AM MDT" || metadata.Schedule.RecentlyCompleted[0].CompletedAt != "Monday July 20, 11:00 AM MDT" {
		t.Fatalf("scheduled item times are not human-readable: %#v", metadata.Schedule)
	}
	if metadata.Schedule.NextLater == nil || metadata.Schedule.NextLater.ID != later.ID {
		t.Fatalf("next item beyond 24 hours was not injected: %#v", metadata.Schedule)
	}
	heartbeat := m.responseEnvelope("[HEARTBEAT | Monday July 20, 12:00 PM MDT | "+timeSinceUser(now, m.lastSent)+"]", now)
	if !strings.Contains(heartbeat, "1h30m0s") || !strings.Contains(heartbeat, "inject due") {
		t.Fatalf("heartbeat did not use refreshed envelope metadata: %q", heartbeat)
	}
	heartbeatJSON := strings.TrimPrefix(heartbeat, "[HARNESS METADATA]\n")
	heartbeatJSON, _, _ = strings.Cut(heartbeatJSON, "\n[/HARNESS METADATA]")
	var heartbeatMetadata responseEnvelopeMetadata
	if err := json.Unmarshal([]byte(heartbeatJSON), &heartbeatMetadata); err != nil {
		t.Fatal(err)
	}
	if heartbeatMetadata.Schedule == nil || len(heartbeatMetadata.Schedule.RecentlyCompleted) != 0 {
		t.Fatalf("completed item was injected more than once in one session: %#v", heartbeatMetadata.Schedule)
	}

	newSession := newAppModel(appConfig{}, envelopeMemory)
	newSessionEnvelope := newSession.responseEnvelope("new session", now)
	newSessionJSON := strings.TrimPrefix(newSessionEnvelope, "[HARNESS METADATA]\n")
	newSessionJSON, _, _ = strings.Cut(newSessionJSON, "\n[/HARNESS METADATA]")
	var newSessionMetadata responseEnvelopeMetadata
	if err := json.Unmarshal([]byte(newSessionJSON), &newSessionMetadata); err != nil {
		t.Fatal(err)
	}
	if newSessionMetadata.Schedule == nil || len(newSessionMetadata.Schedule.RecentlyCompleted) != 1 {
		t.Fatalf("new session did not recover the recent completion once: %#v", newSessionMetadata.Schedule)
	}
	if strings.Contains(newSessionEnvelope, "completion outside history window") {
		t.Fatalf("completion older than 24 hours leaked into metadata: %q", newSessionEnvelope)
	}
}

func TestDiscoverModels(t *testing.T) {
	t.Parallel()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []any{
				map[string]any{
					"name": "gemma4-heretical:latest",
					"details": map[string]any{
						"parameter_size":     "25.2B",
						"quantization_level": "Q4_K_M",
					},
				},
			},
		})
	}))
	defer ollama.Close()
	llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"id": "gemma4-heretical"}},
		})
	}))
	defer llama.Close()

	choices := discoverModels(context.Background(), appConfig{
		OllamaBaseURL: ollama.URL + "/v1",
		LlamaBaseURL:  llama.URL + "/v1",
	})
	if len(choices) != 2 {
		t.Fatalf("got %d choices: %#v", len(choices), choices)
	}
	if !choices[0].Online || !choices[1].Online {
		t.Fatalf("discovered choices should be online: %#v", choices)
	}
}

func TestCompletionToolLoop(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "gemma4-heretical" || len(request.Tools) == 0 || !request.Stream {
			t.Fatalf("request omitted model or tools: %#v", request)
		}
		if request.ReasoningFormat != "deepseek" {
			t.Fatalf("reasoning format = %q, want deepseek", request.ReasoningFormat)
		}
		if calls.Add(1) == 1 {
			writeSSE(w,
				map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
					"role": "assistant", "reasoning_content": "I should ",
					"tool_calls": []any{map[string]any{
						"index": 0, "id": "call-1", "type": "function",
						"function": map[string]any{"name": "get_current_", "arguments": "{"},
					}},
				}}}},
				map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
					"reasoning_content": "use time.",
					"tool_calls": []any{map[string]any{
						"index": 0, "function": map[string]any{"name": "time", "arguments": "}"},
					}},
				}}}},
			)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "call-1" {
			t.Fatalf("tool result was not sent back: %#v", last)
		}
		if len(request.Messages) < 2 || request.Messages[len(request.Messages)-2].ReasoningContent != "I should use time." {
			t.Fatalf("assistant reasoning was not retained across the tool round: %#v", request.Messages)
		}
		writeSSE(w,
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"reasoning_content": "The clock answered.",
			}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "tool loop "}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "works"}}}},
			map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 42}},
		)
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL + "/v1",
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	var (
		result completionResult
		kinds  []completionStreamEventKind
		deltas []string
	)
	for event := range client.completeStream(context.Background(), []apiMessage{
		{Role: "user", Content: "what time is it?"},
	}, true) {
		kinds = append(kinds, event.Kind)
		if event.Delta != "" {
			deltas = append(deltas, event.Delta)
		}
		if event.Kind == streamFinished {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
			result = event.Result
		}
	}
	if result.Reply != "tool loop works" || result.Usage.PromptTokens != 42 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if !result.HadToolCalls {
		t.Fatal("completion did not report its tool call")
	}
	if result.Reasoning != "The clock answered." {
		t.Fatalf("reasoning = %q", result.Reasoning)
	}
	if got := fmt.Sprint(kinds); got != "[0 1 1 3 0 1 2 2 4]" {
		t.Fatalf("stream event order = %s", got)
	}
	if got := strings.Join(deltas, ""); got != "I should use time.The clock answered.tool loop works" {
		t.Fatalf("streamed deltas = %q", got)
	}
}

func TestHeartbeatControlAlongsideVisibleReplyFinishesWithoutAnotherCompletion(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"role":    "assistant",
			"content": "I'll check again in thirty minutes.",
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "heartbeat-1", "type": "function",
				"function": map[string]any{
					"name":      "schedule_next_heartbeat",
					"arguments": `{"when":"30m"}`,
				},
			}},
		}}}})
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	var (
		result completionResult
		kinds  []completionStreamEventKind
	)
	for event := range client.completeStream(context.Background(), []apiMessage{
		{Role: "user", Content: "Check on me later."},
	}, true) {
		kinds = append(kinds, event.Kind)
		if event.Kind == streamFinished {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
			result = event.Result
		}
	}

	if calls.Load() != 1 {
		t.Fatalf("made %d completion requests, want 1", calls.Load())
	}
	if result.Reply != "I'll check again in thirty minutes." || result.NextHeartbeat == nil {
		t.Fatalf("unexpected terminal heartbeat result: %#v", result)
	}
	remaining := time.Until(*result.NextHeartbeat)
	if remaining < 29*time.Minute || remaining > 31*time.Minute {
		t.Fatalf("next heartbeat is %s away, want about 30m", remaining)
	}
	if !result.HadToolCalls || !result.HadVisibleReply {
		t.Fatalf("result did not retain tool/visibility metadata: %#v", result)
	}
	if len(result.Messages) != 1 || result.Messages[0].Role != "user" {
		t.Fatalf("terminal control leaked a dangling tool call into history: %#v", result.Messages)
	}
	if got := fmt.Sprint(kinds); got != "[0 2 4]" {
		t.Fatalf("stream event order = %s, want no tool round", got)
	}
}

func TestTrailingPrintedHeartbeatControlIsAppliedAndRemovedFromReply(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "I'll check again later.\n\n"}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": `schedule_next_heartbeat(when=30m)`}}}},
		)
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	result, err := client.complete(context.Background(), []apiMessage{{Role: "user", Content: "Check later."}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "I'll check again later." || result.NextHeartbeat == nil {
		t.Fatalf("trailing heartbeat directive was not applied cleanly: %#v", result)
	}
	remaining := time.Until(*result.NextHeartbeat)
	if remaining < 29*time.Minute || remaining > 31*time.Minute {
		t.Fatalf("next heartbeat is %s away, want about 30m", remaining)
	}
	if !result.HadVisibleReply || result.HadToolCalls {
		t.Fatalf("fallback directive affected reply/tool metadata: %#v", result)
	}
}

func TestTrailingHeartbeatFallbackIsNarrow(t *testing.T) {
	now := time.Now()
	for _, directive := range []string{
		`{"when":"30m"}`,
		`schedule_next_heartbeat(when="30m")`,
		`schedule_next_heartbeat(when=30m)`,
		`schedule_next_heartbeat( when = '30m' )`,
	} {
		content := "Visible text\n\n" + directive
		visible, when, ok := extractTrailingHeartbeatDirective(content, now)
		if !ok || visible != "Visible text" || when.Sub(now) != 30*time.Minute {
			t.Fatalf("did not parse printed heartbeat directive %q: visible=%q when=%s ok=%t", directive, visible, when, ok)
		}
		if got := hidePotentialTrailingHeartbeatDirective(content); got != "Visible text" {
			t.Fatalf("stream fallback rendered %q for %q", got, directive)
		}
	}

	for _, content := range []string{
		`{"when":"30m"}`,
		"Visible text\n" + `{"when":"invalid"}`,
		"Visible text\n" + `{"when":"30m","other":true}`,
		"Visible text " + `{"when":"30m"}`,
		"Visible text\n" + `schedule_next_heartbeat(when=30m extra)`,
		"Visible text\n" + `schedule_next_heartbeat(when="30m", other=true)`,
	} {
		visible, _, ok := extractTrailingHeartbeatDirective(content, now)
		if ok || visible != content {
			t.Fatalf("unexpectedly consumed %q as a heartbeat directive", content)
		}
	}

}

func TestStructuredHeartbeatNoneIsSilentAndEphemeral(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ToolChoice != "required" {
			t.Errorf("tool choice = %#v, want required", request.ToolChoice)
		}
		if request.ReasoningFormat != "none" {
			t.Errorf("heartbeat reasoning format = %q, want none", request.ReasoningFormat)
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "system" || !strings.HasPrefix(fmt.Sprint(last.Content), heartbeatEventPrefix) {
			t.Errorf("heartbeat event = %#v", last)
		}
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "heartbeat-none", "type": "function",
				"function": map[string]any{"name": "complete_heartbeat", "arguments": `{"action":"none"}`},
			}},
		}}}})
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	base := []apiMessage{{Role: "system", Content: "stable instructions"}, {Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}}
	requestMessages := append(append([]apiMessage(nil), base...), apiMessage{Role: "system", Content: heartbeatEventPrompt(`{"sent_at":"now"}`)})
	var result completionResult
	for event := range client.completeStream(context.Background(), requestMessages, false) {
		if event.Kind == streamFinished {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
			result = event.Result
		}
	}
	if result.Reply != "" || result.HadVisibleReply || len(result.Messages) != len(base) {
		t.Fatalf("silent heartbeat result = %#v", result)
	}
	for _, message := range result.Messages {
		if strings.Contains(fmt.Sprint(message.Content), heartbeatEventPrefix) {
			t.Fatalf("heartbeat event leaked into main history: %#v", result.Messages)
		}
	}
}

func TestStructuredHeartbeatNotifyUsesOnlyValidatedToolMessage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"content": "unvalidated heartbeat prose",
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "heartbeat-notify", "type": "function",
				"function": map[string]any{
					"name":      "complete_heartbeat",
					"arguments": `{"action":"notify","message":"Your package should arrive soon."}`,
				},
			}},
		}}}})
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	base := []apiMessage{{Role: "user", Content: "I am waiting for a package."}}
	messages := append(append([]apiMessage(nil), base...), apiMessage{Role: "system", Content: heartbeatEventPrompt(`{"sent_at":"now"}`)})
	var (
		result       completionResult
		contentDelta bool
	)
	for event := range client.completeStream(context.Background(), messages, false) {
		if event.Kind == streamContentDelta {
			contentDelta = true
		}
		if event.Kind == streamFinished {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
			result = event.Result
		}
	}
	if contentDelta {
		t.Fatal("unvalidated heartbeat prose was streamed to the UI")
	}
	if result.Reply != "Your package should arrive soon." || !result.HadVisibleReply {
		t.Fatalf("structured notification result = %#v", result)
	}
	if len(result.Messages) != len(base) {
		t.Fatalf("heartbeat protocol leaked into returned history: %#v", result.Messages)
	}
}

func TestHeartbeatDecisionValidation(t *testing.T) {
	t.Parallel()
	for name, arguments := range map[string]string{
		"none with message": `{"action":"none","message":"should not appear"}`,
		"notify empty":      `{"action":"notify"}`,
		"unknown action":    `{"action":"later"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseHeartbeatDecision(toolCall{Function: functionCall{Arguments: arguments}})
			if err == nil {
				t.Fatalf("accepted invalid heartbeat decision %s", arguments)
			}
		})
	}
	decision, err := parseHeartbeatDecision(toolCall{Function: functionCall{Arguments: `{"action":"notify","message":"  Time to check in.  "}`}})
	if err != nil || decision.Message != "Time to check in." {
		t.Fatalf("valid heartbeat decision = %#v, %v", decision, err)
	}
}

func TestInvalidHeartbeatControlStillGetsAToolResultRound(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if calls.Add(1) == 1 {
			writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"role":    "assistant",
				"content": "I'll check again later.",
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "heartbeat-invalid", "type": "function",
					"function": map[string]any{
						"name":      "schedule_next_heartbeat",
						"arguments": `{"when":"not-a-time"}`,
					},
				}},
			}}}})
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || !strings.Contains(fmt.Sprint(last.Content), "Could not schedule") {
			t.Fatalf("invalid control result was not returned to the model: %#v", last)
		}
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"content": "I couldn't choose that heartbeat time.",
		}}}})
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	result, err := client.complete(context.Background(), []apiMessage{{Role: "user", Content: "Check later."}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.NextHeartbeat != nil {
		t.Fatalf("invalid control made %d requests with heartbeat %v", calls.Load(), result.NextHeartbeat)
	}
	if result.Reply != "I couldn't choose that heartbeat time." {
		t.Fatalf("repair reply = %q", result.Reply)
	}
}

func TestCompletionPausesForImageRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "image-1", "type": "function",
				"function": map[string]any{
					"name":      "request_image",
					"arguments": `{"prompt":"Photograph the warning label","reason":"I need to read its voltage rating"}`,
				},
			}},
		}}}})
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	result, err := client.complete(context.Background(), []apiMessage{{Role: "user", Content: "Help identify this adapter."}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("made %d API calls; image consent should pause after one", calls.Load())
	}
	if result.ImageRequest == nil {
		t.Fatal("completion did not surface the image request")
	}
	if result.ImageRequest.ToolCallID != "image-1" || result.ImageRequest.Prompt != "Photograph the warning label" {
		t.Fatalf("unexpected image request: %#v", result.ImageRequest)
	}
	if len(result.Messages) != 2 || len(result.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool call was not retained: %#v", result.Messages)
	}
}

func writeSSE(w http.ResponseWriter, chunks ...any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestStartupLoadsContextAndConfiguresLabels(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	if _, err := store.Save("The user likes astronomical names.", []string{"preference"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRecentMessage("user", "previous question"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRecentMessage("assistant", "previous answer"); err != nil {
		t.Fatal(err)
	}
	systemFile := filepath.Join(t.TempDir(), "startup.md")
	if err := os.WriteFile(systemFile, []byte("You are a careful local companion."), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, recent, memoryCount, err := store.InitialSystemMessages(systemFile)
	if err != nil {
		t.Fatal(err)
	}
	if memoryCount != 1 {
		t.Fatalf("loaded %d memories, want 1", memoryCount)
	}
	if len(recent) != 2 || recent[0].Content != "previous question" || recent[1].Content != "previous answer" {
		t.Fatalf("startup recent messages = %#v", recent)
	}
	if len(initial) == 0 || initial[0].Content != "You are a careful local companion." {
		t.Fatalf("startup file was not the first system message: %#v", initial)
	}
	for index, message := range initial {
		if message.Role != "system" {
			t.Fatalf("initial message %d has role %q, want system", index, message.Role)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "configure_session_labels" {
			t.Fatalf("unexpected startup tools: %#v", request.Tools)
		}
		encoded, _ := json.Marshal(request.Messages)
		context := string(encoded)
		for _, expected := range []string{
			"careful local companion",
			"astronomical names",
			"previous question",
			"HARNESS PROTOCOL",
			"HARNESS REQUEST: session_labels",
		} {
			if !strings.Contains(context, expected) {
				t.Errorf("startup request omitted %q: %s", expected, context)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":   "labels-1",
						"type": "function",
						"function": map[string]any{
							"name":      "configure_session_labels",
							"arguments": `{"assistant_label":"Nova","user_label":"Stargazer"}`,
						},
					}},
				},
				},
			},
			"usage": map[string]any{"prompt_tokens": 321},
		})
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, store, 5*time.Second)
	result, err := client.startup(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	if result.Labels.Assistant != "Nova" || result.Labels.User != "Stargazer" {
		t.Fatalf("unexpected startup labels: %#v", result.Labels)
	}
	if result.Usage.PromptTokens != 321 {
		t.Fatalf("startup prompt tokens = %d", result.Usage.PromptTokens)
	}
	encoded, _ := json.Marshal(result.Messages)
	if !strings.Contains(string(encoded), "HARNESS CONFIGURATION") || !strings.Contains(string(encoded), "Stargazer") {
		t.Fatalf("accepted harness exchange was not retained: %s", encoded)
	}
}

func TestStartupRetriesProseResponse(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{
					"role": "assistant", "content": "Call me Nova.",
				}}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "labels-2", "type": "function",
					"function": map[string]any{
						"name":      "configure_session_labels",
						"arguments": `{"assistant_label":"Nova","user_label":"Friend"}`,
					},
				}},
			}}},
		})
	}))
	defer server.Close()
	client := newAPIClient(modelChoice{
		Backend: backendLlama, Model: "gemma4-heretical", BaseURL: server.URL, APIKey: "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	result, err := client.startup(context.Background(), []apiMessage{{Role: "system", Content: harnessProtocolInstructions}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.Labels.Assistant != "Nova" {
		t.Fatalf("startup retry failed: calls=%d result=%#v", calls.Load(), result)
	}
}

func TestStartupProtocolExchangeIsDebugVisible(t *testing.T) {
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.phase = phaseStarting
	m.startupSession = 4
	m.selected = modelChoice{Backend: backendLlama, Model: "gemma4-heretical"}
	m.width = 80
	m.height = 30
	m.resize()
	m.messages = []apiMessage{
		{Role: "system", Content: "private startup instructions"},
		{Role: "system", Content: harnessProtocolInstructions},
	}
	m.restoredTranscript = []transcriptMessage{
		{Role: "user", Content: "previous question", OccurredAt: time.Now(), Meta: "restored"},
		{Role: "assistant", Content: "previous answer", OccurredAt: time.Now(), Meta: "restored"},
	}
	completed := append([]apiMessage(nil), m.messages...)
	completed = append(completed,
		apiMessage{Role: "system", Content: sessionLabelRequest},
		apiMessage{
			Role: "assistant",
			ToolCalls: []toolCall{{
				ID:   "labels-1",
				Type: "function",
				Function: functionCall{
					Name:      "configure_session_labels",
					Arguments: `{"assistant_label":"Nova","user_label":"Stargazer"}`,
				},
			}},
		},
		apiMessage{Role: "tool", ToolCallID: "labels-1", Content: `{"accepted":true,"request":"session_labels"}`},
		apiMessage{Role: "system", Content: "[HARNESS COMPLETE: session_labels]"},
		apiMessage{Role: "system", Content: `[HARNESS CONFIGURATION]\nUse "Nova" and "Stargazer".`},
	)

	updated, _ := m.Update(startupMsg{
		session: 4,
		result: startupResult{
			Messages: completed,
			Labels:   sessionLabels{Assistant: "Nova", User: "Stargazer"},
		},
	})
	m = updated.(appModel)

	rendered := m.renderTranscript(72)
	if strings.Contains(rendered, "HARNESS REQUEST") || strings.Contains(rendered, "configure_session_labels") {
		t.Fatalf("startup exchange leaked into the normal transcript: %q", rendered)
	}
	var restored []transcriptMessage
	for _, message := range m.transcript {
		if message.Meta == "restored" {
			restored = append(restored, message)
		}
	}
	if len(restored) != 2 || restored[0].Role != "user" || restored[0].Content != "previous question" || restored[1].Role != "assistant" || restored[1].Content != "previous answer" {
		t.Fatalf("normal transcript omitted restored history: %#v", restored)
	}
	m.showDebug = true
	rendered = m.renderTranscript(72)
	for _, expected := range []string{
		"HARNESS PROTOCOL",
		"HARNESS REQUEST: session_labels",
		"configure_session_labels",
		`"assistant_label":"Nova"`,
		`"accepted":true`,
		"HARNESS COMPLETE: session_labels",
		"HARNESS CONFIGURATION",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("debug transcript omitted %q: %q", expected, rendered)
		}
	}
	if strings.Contains(rendered, "private startup instructions") {
		t.Fatalf("unrelated startup context leaked into the protocol debug view: %q", rendered)
	}
}

func TestSessionLabelValidation(t *testing.T) {
	t.Parallel()
	_, err := parseSessionLabels(toolCall{Function: functionCall{Arguments: `{"assistant_label":"same","user_label":"SAME"}`}})
	if err == nil {
		t.Fatal("accepted duplicate session labels")
	}
	_, err = parseSessionLabels(toolCall{Function: functionCall{Arguments: `{"assistant_label":"bad\nlabel","user_label":"User"}`}})
	if err == nil {
		t.Fatal("accepted a control character in a terminal label")
	}
}

func TestTranscriptUsesSessionLabels(t *testing.T) {
	t.Parallel()
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.width = 80
	m.userLabel = "Stargazer"
	m.assistantLabel = "Nova"
	m.transcript = []transcriptMessage{
		{Role: "user", Content: "hello", OccurredAt: time.Now()},
		{Role: "assistant", Content: "hi", OccurredAt: time.Now()},
	}
	rendered := m.renderTranscript(72)
	if !strings.Contains(rendered, "Stargazer") || !strings.Contains(rendered, "Nova") {
		t.Fatalf("session labels missing from transcript: %q", rendered)
	}
}

func TestTranscriptHidesSystemMessagesUnlessDebugging(t *testing.T) {
	t.Parallel()
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.transcript = []transcriptMessage{
		{Role: "system", Content: "internal connection details", OccurredAt: time.Now()},
		{Role: "reasoning", Content: "private chain of thought", OccurredAt: time.Now()},
		{Role: "user", Content: "visible user turn", OccurredAt: time.Now()},
		{Role: "assistant", Content: "visible assistant turn", OccurredAt: time.Now()},
		{Role: "notice", Content: "explicitly requested help", OccurredAt: time.Now()},
		{Role: "error", Content: "visible failure", OccurredAt: time.Now()},
	}

	rendered := m.renderTranscript(72)
	for _, hidden := range []string{"internal connection details", "private chain of thought"} {
		if strings.Contains(rendered, hidden) {
			t.Fatalf("debug message %q leaked into normal transcript: %q", hidden, rendered)
		}
	}
	for _, visible := range []string{"visible user turn", "visible assistant", "explicitly requested help", "visible failure"} {
		if !strings.Contains(rendered, visible) {
			t.Fatalf("normal transcript omitted %q: %q", visible, rendered)
		}
	}

	m.showDebug = true
	rendered = m.renderTranscript(72)
	for _, visible := range []string{"internal connection details", "private chain of thought", "THINKING"} {
		if !strings.Contains(rendered, visible) {
			t.Fatalf("debug transcript omitted %q: %q", visible, rendered)
		}
	}
}

func TestCompletionStreamUpdatesTUIIncrementally(t *testing.T) {
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.phase = phaseChatting
	m.width = 80
	m.height = 30
	m.resize()
	m.busy = true
	m.streamSession = 7
	m.streamPrompt = "question"
	m.streamEvents = make(chan completionStreamEvent)
	m.deliveryStage = deliverySending

	updated, _ := m.Update(completionStreamMsg{
		session: 7,
		events:  m.streamEvents,
		event:   completionStreamEvent{Kind: streamConnected},
		ok:      true,
	})
	m = updated.(appModel)
	if m.deliveryStage != deliveryDelivered || !strings.Contains(m.renderStatus(), "Delivered ✓") {
		t.Fatalf("connected stream did not enter delivered state: %#v", m)
	}

	updated, _ = m.Update(completionStreamMsg{
		session: 7,
		events:  m.streamEvents,
		event:   completionStreamEvent{Kind: streamReasoningDelta, Delta: "considering"},
		ok:      true,
	})
	m = updated.(appModel)
	if m.streamReasoning != "considering" || m.deliveryStage != deliveryDelivered {
		t.Fatalf("reasoning delta was not reflected in the TUI: %#v", m)
	}
	updated, _ = m.Update(deliveryTypingMsg{session: 7, sequence: m.deliverySequence})
	m = updated.(appModel)
	if m.deliveryStage != deliveryTyping || !strings.Contains(m.renderStatus(), "Typing…") {
		t.Fatalf("delivered stream did not advance to typing state: %#v", m)
	}

	updated, _ = m.Update(completionStreamMsg{
		session: 7,
		events:  m.streamEvents,
		event:   completionStreamEvent{Kind: streamContentDelta, Delta: "answer"},
		ok:      true,
	})
	m = updated.(appModel)
	if m.streamAnswer != "answer" || m.deliveryStage != deliveryTyping || m.status != "Typing…" {
		t.Fatalf("answer delta was not reflected in the TUI: %#v", m)
	}

	updated, _ = m.Update(completionStreamMsg{
		session: 7,
		events:  m.streamEvents,
		event: completionStreamEvent{Kind: streamFinished, Result: completionResult{
			Reply: "answer", Reasoning: "considering",
			Messages: []apiMessage{{Role: "user", Content: "question"}},
		}},
		ok: true,
	})
	m = updated.(appModel)
	if m.busy || m.streamAnswer != "" || m.streamReasoning != "" {
		t.Fatalf("finished stream was not cleared: %#v", m)
	}
	if len(m.messages) != 2 || m.messages[1].ReasoningContent != "considering" {
		t.Fatalf("final reasoning was not preserved in API history: %#v", m.messages)
	}
	if len(m.transcript) != 2 || m.transcript[0].Role != "reasoning" || m.transcript[1].Role != "assistant" {
		t.Fatalf("finished stream transcript is wrong: %#v", m.transcript)
	}
}

func TestSilentStructuredHeartbeatRearmsWithoutChangingHistory(t *testing.T) {
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval}, testMemoryStore(t))
	m.phase = phaseChatting
	m.width = 80
	m.height = 30
	m.resize()
	base := []apiMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}}
	m.messages = append([]apiMessage(nil), base...)
	m.busy = true
	m.streamSession = 7
	m.streamEvents = make(chan completionStreamEvent)
	m.streamHeartbeat = true

	updated, _ := m.Update(completionStreamMsg{
		session: 7,
		events:  m.streamEvents,
		event: completionStreamEvent{
			Kind:   streamFinished,
			Result: completionResult{Messages: base, HadToolCalls: true},
		},
		ok: true,
	})
	m = updated.(appModel)
	if !m.heartbeatArmed {
		t.Fatal("silent heartbeat did not schedule the next heartbeat")
	}
	if len(m.messages) != len(base) || len(m.transcript) != 0 {
		t.Fatalf("silent heartbeat changed main history: messages=%#v transcript=%#v", m.messages, m.transcript)
	}
}

func TestHeartbeatNotificationPersistsOnlyAssistantMessage(t *testing.T) {
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval}, testMemoryStore(t))
	m.phase = phaseChatting
	base := []apiMessage{{Role: "user", Content: "Check in later."}, {Role: "assistant", Content: "I will."}}
	m.messages = append([]apiMessage(nil), base...)
	reply := "Your scheduled item is due now."
	updated, _ := m.Update(responseMsg{heartbeat: true, result: completionResult{
		Reply:           reply,
		Messages:        base,
		HadVisibleReply: true,
		HadToolCalls:    true,
	}})
	m = updated.(appModel)
	if m.heartbeatArmed {
		t.Fatal("heartbeat re-armed after a visible reply")
	}
	if len(m.transcript) != 1 || m.transcript[0].Content != reply {
		t.Fatalf("heartbeat notification was not surfaced: %#v", m.transcript)
	}
	if len(m.messages) != len(base)+1 || m.messages[len(base)].Role != "assistant" || m.messages[len(base)].Content != reply {
		t.Fatalf("heartbeat protocol leaked or notification was not persisted: %#v", m.messages)
	}
}

func TestHeartbeatFailureIncludesCauseInDebugStatus(t *testing.T) {
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval, Debug: true}, testMemoryStore(t))
	updated, _ := m.Update(responseMsg{
		heartbeat: true,
		err:       errors.New("tool call limit exceeded"),
	})
	m = updated.(appModel)
	if !strings.Contains(m.status, "heartbeat failed: tool call limit exceeded") {
		t.Fatalf("heartbeat failure status omitted cause: %q", m.status)
	}
	if !m.heartbeatArmed {
		t.Fatal("failed heartbeat did not schedule another check")
	}
}

func TestCameraPreviewWithFakeImageSnap(t *testing.T) {
	framePath := filepath.Join(t.TempDir(), "frame.jpg")
	frameFile, err := os.Create(framePath)
	if err != nil {
		t.Fatal(err)
	}
	frame := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for y := range 4 {
		for x := range 8 {
			frame.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 60), B: 120, A: 255})
		}
	}
	if err := jpeg.Encode(frameFile, frame, nil); err != nil {
		frameFile.Close()
		t.Fatal(err)
	}
	if err := frameFile.Close(); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	backend := filepath.Join(bin, "imagesnap")
	script := `#!/bin/sh
for output do :; done
/bin/cp "$FAKE_CAMERA_FRAME" "$output"
`
	if err := os.WriteFile(backend, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_CAMERA_FRAME", framePath)

	previews := filepath.Join(t.TempDir(), "previews")
	path, ansi, err := capturePreviewFrame(context.Background(), previews, 8, 4)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != previews || filepath.Ext(path) != ".jpg" {
		t.Fatalf("unexpected preview path: %q", path)
	}
	if !strings.Contains(ansi, "\x1b[38;2;") || !strings.Contains(ansi, "▀") {
		t.Fatalf("preview was not rendered as true-color terminal pixels: %q", ansi)
	}
}

func TestKeepPreviewFramePromotesExactImage(t *testing.T) {
	store := testMemoryStore(t)
	previewDir := filepath.Join(store.dir, "camera-preview")
	if err := os.MkdirAll(previewDir, 0o700); err != nil {
		t.Fatal(err)
	}
	preview := filepath.Join(previewDir, "preview.jpg")
	if err := os.WriteFile(preview, []byte("shown-frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := newCaptureVault()
	m := appModel{memory: store, captureVault: vault, previewFrame: preview, previewSession: 1}
	captured, err := m.keepPreviewFrame()
	if err != nil {
		t.Fatal(err)
	}
	content, err := vault.Open(captured)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(content)
	if string(content) != "shown-frame" {
		t.Fatalf("captured frame differs from preview: %q", content)
	}
	if _, err := os.Stat(preview); !os.IsNotExist(err) {
		t.Fatalf("preview was not moved into captures: %v", err)
	}
}

func TestCapturePhotoWindowReturnsBrowserJPEG(t *testing.T) {
	photo := testJPEG(t, 24, 16)
	requestDone := make(chan error, 1)
	opener := func(url string) error {
		go func() {
			response, err := http.Get(url)
			if err != nil {
				requestDone <- err
				return
			}
			page, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				requestDone <- readErr
				return
			}
			if response.StatusCode != http.StatusOK || !bytes.Contains(page, []byte("getUserMedia")) {
				requestDone <- fmt.Errorf("unexpected camera page: status=%s", response.Status)
				return
			}
			request, err := http.NewRequest(http.MethodPost, url+"capture", bytes.NewReader(photo))
			if err != nil {
				requestDone <- err
				return
			}
			request.Header.Set("Content-Type", "image/jpeg")
			response, err = http.DefaultClient.Do(request)
			if err != nil {
				requestDone <- err
				return
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				requestDone <- fmt.Errorf("capture returned %s", response.Status)
				return
			}
			requestDone <- nil
		}()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	directory := filepath.Join(t.TempDir(), "captures")
	vault := newCaptureVault()
	path, cancelled, err := capturePhotoWindow(ctx, directory, vault, opener)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Fatal("browser capture was unexpectedly cancelled")
	}
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(stored, photo) || bytes.Contains(stored, photo[:min(32, len(photo))]) {
		t.Fatal("browser photo was stored as recoverable plaintext")
	}
	decrypted, err := vault.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(decrypted)
	if !bytes.Equal(decrypted, photo) {
		t.Fatal("decrypted browser photo differs from submitted JPEG")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("browser photo permissions = %v", info.Mode().Perm())
	}
}

func TestBrowserBundleIDFromLaunchServices(t *testing.T) {
	t.Parallel()
	preferences := `(
    {
        LSHandlerRoleAll = "com.apple.Safari";
        LSHandlerURLScheme = ftp;
    },
    {
        LSHandlerPreferredVersions = { LSHandlerRoleAll = "-"; };
        LSHandlerRoleAll = "org.mozilla.firefox";
        LSHandlerURLScheme = http;
    },
    {
        LSHandlerRoleAll = "org.mozilla.firefox";
        LSHandlerURLScheme = https;
    }
)`
	if got := browserBundleIDFromLaunchServices(preferences, "http"); got != "org.mozilla.firefox" {
		t.Fatalf("default HTTP browser = %q, want org.mozilla.firefox", got)
	}
}

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			frame.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 11), B: 120, A: 255})
		}
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, frame, nil); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestFinishImageRequestAddsApprovedImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nphoto"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newAppModel(appConfig{RequestTimeout: time.Second}, testMemoryStore(t))
	m.phase = phasePhotoReview
	m.imageRequest = &imageRequest{
		ToolCallID: "image-1",
		Prompt:     "Show me the label",
		Reason:     "Read the voltage",
	}
	m.messages = []apiMessage{{
		Role: "assistant",
		ToolCalls: []toolCall{{
			ID:       "image-1",
			Function: functionCall{Name: "request_image"},
		}},
	}}
	if cmd := m.finishImageRequest(path, false); cmd == nil {
		t.Fatal("approval did not schedule the model continuation")
	}
	if len(m.messages) != 3 {
		t.Fatalf("got %d messages, want assistant call, tool result, and image", len(m.messages))
	}
	if m.messages[1].Role != "tool" || m.messages[1].ToolCallID != "image-1" {
		t.Fatalf("missing tool approval: %#v", m.messages[1])
	}
	parts, ok := m.messages[2].Content.([]contentPart)
	if !ok || len(parts) != 2 || parts[1].ImageURL == nil {
		t.Fatalf("approved image was not added as multimodal user content: %#v", m.messages[2])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("user-selected image was deleted: %v", err)
	}
}

func TestFinishImageRequestDeletesOwnedCameraCapture(t *testing.T) {
	m := newAppModel(appConfig{RequestTimeout: time.Second}, testMemoryStore(t))
	path, err := m.captureVault.SealJPEG(t.TempDir(), testJPEG(t, 12, 8))
	if err != nil {
		t.Fatal(err)
	}
	m.phase = phasePhotoReview
	m.capturePath = path
	m.captureOwned = true
	m.imageRequest = &imageRequest{ToolCallID: "image-1", Prompt: "Show me the label", Reason: "Read it"}
	m.messages = []apiMessage{{Role: "assistant", ToolCalls: []toolCall{{ID: "image-1"}}}}

	if cmd := m.finishImageRequest(path, false); cmd == nil {
		t.Fatal("approval did not schedule the model continuation")
	}
	m.stopCompletionStream()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owned camera capture still exists after encoding: %v", err)
	}
}

func TestManualCameraCaptureIsDeletedWhenQueuedMessageIsSent(t *testing.T) {
	m := newAppModel(appConfig{RequestTimeout: time.Second}, testMemoryStore(t))
	path, err := m.captureVault.SealJPEG(t.TempDir(), testJPEG(t, 12, 8))
	if err != nil {
		t.Fatal(err)
	}
	m.phase = phasePhotoReview
	m.captureManual = true
	m.capturePath = path
	m.captureOwned = true
	m.imageRequest = &imageRequest{Prompt: "Take a photo", Reason: "Manual capture"}

	if cmd := m.finishImageRequest(path, false); cmd == nil {
		t.Fatal("manual approval did not restore composer focus")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("queued capture was deleted before send: %v", err)
	}
	if cmd := m.submit("What do you see?"); cmd == nil {
		t.Fatal("submit did not schedule a completion")
	}
	m.stopCompletionStream()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("queued camera capture still exists after encoding: %v", err)
	}
}

func TestDiscardEmbeddedImagesRemovesDataURLs(t *testing.T) {
	messages := []apiMessage{{
		Role: "user",
		Content: []contentPart{
			{Type: "text", Text: "What is this?"},
			{Type: "image_url", ImageURL: &imageURL{URL: "data:image/png;base64,secret-payload"}},
		},
	}}
	redacted := discardEmbeddedImages(messages)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret-payload")) || bytes.Contains(encoded, []byte("data:image")) {
		t.Fatalf("embedded image survived redaction: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte("Image data discarded")) {
		t.Fatalf("redaction marker missing: %s", encoded)
	}
}

func TestCaptureVaultClearsPerCaptureKeyOnRemoval(t *testing.T) {
	vault := newCaptureVault()
	plaintext := testJPEG(t, 12, 8)
	path, err := vault.SealJPEG(t.TempDir(), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	record := vault.files[path]
	if record == nil {
		t.Fatal("capture key was not retained for decryption")
	}
	if err := vault.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("encrypted capture still exists after removal: %v", err)
	}
	for index, value := range record.bytes {
		if value != 0 {
			t.Fatalf("capture key byte %d was not cleared", index)
		}
	}
}

func TestShutdownCleanupDeletesUnsentCameraFiles(t *testing.T) {
	dir := t.TempDir()
	preview := filepath.Join(dir, "preview.jpg")
	review := filepath.Join(dir, "review.jpg")
	for _, path := range []string{preview, review} {
		if err := os.WriteFile(path, []byte("camera"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newAppModel(appConfig{}, testMemoryStore(t))
	queued, err := m.captureVault.SealJPEG(dir, []byte("queued camera"))
	if err != nil {
		t.Fatal(err)
	}
	m.previewFrame = preview
	m.capturePath = review
	m.captureOwned = true
	m.pendingImages = []string{queued}

	if err := m.cleanupOwnedImages(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{preview, review, queued} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("camera file still exists after shutdown cleanup: %s (%v)", path, err)
		}
	}
}

func TestImageRequestContinuationRestoresComposerFocus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nphoto"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newAppModel(appConfig{RequestTimeout: time.Second}, testMemoryStore(t))
	m.phase = phasePhotoReview
	m.imageRequest = &imageRequest{
		ToolCallID: "image-1",
		Prompt:     "Show me the label",
		Reason:     "Read the voltage",
	}
	m.messages = []apiMessage{{
		Role: "assistant",
		ToolCalls: []toolCall{{
			ID:       "image-1",
			Function: functionCall{Name: "request_image"},
		}},
	}}
	m.composer.Blur()
	if cmd := m.finishImageRequest(path, false); cmd == nil {
		t.Fatal("approval did not schedule the model continuation")
	}
	if m.composer.Focused() {
		t.Fatal("composer unexpectedly focused while image continuation was running")
	}

	updated, _ := m.Update(responseMsg{
		result: completionResult{Reply: "The label says 12 volts.", Messages: m.messages},
		prompt: "Read this label",
	})
	got := updated.(appModel)
	got.stopCompletionStream()
	if !got.composer.Focused() {
		t.Fatal("composer did not regain focus after the image continuation completed")
	}
}

func TestDiscardRemovesOnlyOwnedCapture(t *testing.T) {
	owned := filepath.Join(t.TempDir(), "camera.jpg")
	if err := os.WriteFile(owned, []byte("camera"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := appModel{capturePath: owned, captureOwned: true}
	m.discardOwnedCapture()
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("discarded camera capture still exists: %v", err)
	}

	selected := filepath.Join(t.TempDir(), "selected.jpg")
	if err := os.WriteFile(selected, []byte("selected"), 0o600); err != nil {
		t.Fatal(err)
	}
	m = appModel{capturePath: selected, captureOwned: false}
	m.discardOwnedCapture()
	if _, err := os.Stat(selected); err != nil {
		t.Fatalf("discard removed a user-selected file: %v", err)
	}
}

func TestInitialSystemMessages(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	if _, err := store.Save("likes concise answers", []string{"preference"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRecentMessage("user", "last question"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRecentMessage("assistant", "last answer"); err != nil {
		t.Fatal(err)
	}
	messages, recent, count, err := store.InitialSystemMessages("")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(messages) < 3 {
		t.Fatalf("unexpected initial context: count=%d messages=%#v", count, messages)
	}
	transcript := recentMessagesTranscript(recent)
	if len(transcript) != 2 || transcript[0].Role != "user" || transcript[0].Content != "last question" || transcript[0].Meta != "restored" || transcript[1].Role != "assistant" || transcript[1].Content != "last answer" {
		t.Fatalf("unexpected restored transcript: %#v", transcript)
	}
}

func TestLastUserActivityPersistsAcrossSessions(t *testing.T) {
	store := testMemoryStore(t)
	want := time.Date(2026, 7, 20, 11, 22, 33, 456, time.FixedZone("MDT", -6*60*60))
	if err := store.RecordUserActivity(want); err != nil {
		t.Fatal(err)
	}
	if got := store.LastUserActivityTime(); !got.Equal(want) {
		t.Fatalf("last user activity = %s, want %s", got, want)
	}
}

func TestRecentMessagesRollOverAtFive(t *testing.T) {
	store := testMemoryStore(t)
	roles := []string{"user", "assistant", "user", "assistant", "user", "assistant", "user"}
	for index, role := range roles {
		if err := store.AppendRecentMessage(role, fmt.Sprintf("message %d", index+1)); err != nil {
			t.Fatal(err)
		}
	}

	messages, err := store.RecentMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != recentMessageLimit {
		t.Fatalf("retained %d messages, want %d: %#v", len(messages), recentMessageLimit, messages)
	}
	for index, message := range messages {
		wantContent := fmt.Sprintf("message %d", index+3)
		if message.Content != wantContent || message.Role != roles[index+2] {
			t.Errorf("message %d = %#v, want role=%q content=%q", index, message, roles[index+2], wantContent)
		}
	}

	initial, _, _, err := store.InitialSystemMessages("")
	if err != nil {
		t.Fatal(err)
	}
	context := ""
	for _, message := range initial {
		context += fmt.Sprint(message.Content)
	}
	if strings.Contains(context, "message 2") || !strings.Contains(context, "message 3") || !strings.Contains(context, "message 7") {
		t.Fatalf("startup context did not contain only the rolling window: %s", context)
	}
}

func TestLegacyLastMessageToolsAreNotExposed(t *testing.T) {
	for _, tool := range tools {
		switch tool.Function.Name {
		case "get_last_message", "get_last_response":
			t.Fatalf("legacy tool %q is still exposed", tool.Function.Name)
		}
	}
}

func TestTypingDoesNotScrollTranscript(t *testing.T) {
	store := testMemoryStore(t)
	m := newAppModel(appConfig{}, store)
	m.phase = phaseChatting
	m.width = 80
	m.height = 24
	m.resize()
	m.viewport.SetContent(strings.Repeat("history line\n", 40))
	m.viewport.GotoBottom()
	wantOffset := m.viewport.YOffset()
	m.composer.Focus()

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Text: "j", Code: 'j'}))
	got := updated.(appModel)
	if got.viewport.YOffset() != wantOffset {
		t.Fatalf("typing j moved transcript from offset %d to %d", wantOffset, got.viewport.YOffset())
	}
	if got.composer.Value() != "j" {
		t.Fatalf("composer value = %q, want j", got.composer.Value())
	}
}

func TestControlLDoesNotClearTranscript(t *testing.T) {
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.phase = phaseChatting
	m.transcript = []transcriptMessage{{Role: "assistant", Content: "keep this visible"}}
	m.composer.Focus()

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: 'l', Mod: tea.ModCtrl}))
	got := updated.(appModel)
	if len(got.transcript) != 1 || got.transcript[0].Content != "keep this visible" {
		t.Fatalf("Ctrl+L changed the transcript: %#v", got.transcript)
	}
}

func TestPageUpStillScrollsTranscript(t *testing.T) {
	store := testMemoryStore(t)
	m := newAppModel(appConfig{}, store)
	m.phase = phaseChatting
	m.width = 80
	m.height = 24
	m.resize()
	m.viewport.SetContent(strings.Repeat("history line\n", 40))
	m.viewport.GotoBottom()
	wantLessThan := m.viewport.YOffset()
	m.composer.Focus()

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgUp}))
	got := updated.(appModel)
	if got.viewport.YOffset() >= wantLessThan {
		t.Fatalf("page up did not move transcript above offset %d: got %d", wantLessThan, got.viewport.YOffset())
	}
	if got.composer.Value() != "" {
		t.Fatalf("page up unexpectedly changed composer to %q", got.composer.Value())
	}
}

func TestMouseWheelStillScrollsTranscript(t *testing.T) {
	store := testMemoryStore(t)
	m := newAppModel(appConfig{}, store)
	m.phase = phaseChatting
	m.width = 80
	m.height = 24
	m.resize()
	m.viewport.SetContent(strings.Repeat("history line\n", 40))
	m.viewport.GotoBottom()
	wantLessThan := m.viewport.YOffset()
	m.composer.Focus()

	updated, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	got := updated.(appModel)
	if got.viewport.YOffset() >= wantLessThan {
		t.Fatalf("mouse wheel did not move transcript above offset %d: got %d", wantLessThan, got.viewport.YOffset())
	}
	if got.composer.Value() != "" {
		t.Fatalf("mouse wheel unexpectedly changed composer to %q", got.composer.Value())
	}
}

func TestViewEnablesMouseWheelEvents(t *testing.T) {
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.phase = phaseChatting
	m.width = 80
	m.height = 24
	m.resize()

	view := m.View()
	if view.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode = %v, want cell motion", view.MouseMode)
	}
}

func TestHeartbeatRepeatsUntilAgentSendsVisibleReplyThenWaitsForUser(t *testing.T) {
	now := time.Now()
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval, RequestTimeout: time.Second}, testMemoryStore(t))
	m.phase = phaseChatting
	m.lastSent = now
	if cmd := m.armDefaultHeartbeat(now); cmd == nil {
		t.Fatal("default heartbeat was not armed")
	}
	firstGeneration := m.heartbeatGeneration

	updated, _ := m.Update(heartbeatMsg{generation: firstGeneration, at: now.Add(defaultHeartbeatInterval)})
	m = updated.(appModel)
	if m.heartbeatArmed || !m.busy {
		t.Fatalf("due heartbeat state = armed %t, busy %t", m.heartbeatArmed, m.busy)
	}
	m.stopCompletionStream()
	updated, _ = m.Update(responseMsg{heartbeat: true, result: completionResult{Reply: ""}})
	m = updated.(appModel)
	if !m.heartbeatArmed {
		t.Fatal("silent heartbeat did not schedule another check")
	}

	secondGeneration := m.heartbeatGeneration
	updated, _ = m.Update(heartbeatMsg{generation: secondGeneration, at: now.Add(2 * defaultHeartbeatInterval)})
	m = updated.(appModel)
	m.stopCompletionStream()
	updated, _ = m.Update(responseMsg{heartbeat: true, result: completionResult{Reply: "I followed up."}})
	m = updated.(appModel)
	if m.heartbeatArmed {
		t.Fatal("heartbeat remained armed after a visible assistant reply")
	}

	if cmd := m.submit("Thanks"); cmd == nil {
		t.Fatal("user response did not schedule work")
	}
	m.stopCompletionStream()
	if !m.heartbeatArmed {
		t.Fatal("user response did not re-arm heartbeats")
	}
}

func TestRegularReplyCanOverrideNextHeartbeat(t *testing.T) {
	now := time.Now()
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval}, testMemoryStore(t))
	m.phase = phaseChatting
	_ = m.armDefaultHeartbeat(now)
	defaultGeneration := m.heartbeatGeneration
	target := now.Add(2 * time.Hour)

	updated, _ := m.Update(responseMsg{result: completionResult{
		Reply:         "I'll check later.",
		NextHeartbeat: &target,
	}})
	m = updated.(appModel)
	if !m.heartbeatArmed || !m.heartbeatDue.Equal(target) {
		t.Fatalf("heartbeat override = armed %t due %s, want %s", m.heartbeatArmed, m.heartbeatDue, target)
	}
	if !m.heartbeatExplicit {
		t.Fatal("agent heartbeat override was not marked explicit")
	}
	if m.heartbeatGeneration == defaultGeneration {
		t.Fatal("heartbeat override did not invalidate the previous timer")
	}
}

func TestDebugStatusShowsNextHeartbeatTimeAndScheduledMarker(t *testing.T) {
	now := time.Now()
	due := now.Add(2 * time.Hour).Truncate(time.Second)
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval}, testMemoryStore(t))
	m.status = "Ready"
	m.showDebug = true

	_ = m.armHeartbeatAt(due)
	status := m.renderStatus()
	wantDefault := "Next heartbeat: " + due.Local().Format("3:04 PM")
	if !strings.Contains(status, wantDefault) || strings.Contains(status, "[scheduled]") {
		t.Fatalf("default heartbeat debug status is incomplete: %q", status)
	}

	_ = m.armExplicitHeartbeatAt(due)
	status = m.renderStatus()
	if !strings.Contains(status, wantDefault+" [scheduled]") {
		t.Fatalf("explicit heartbeat debug status is incomplete: %q", status)
	}

	m.showDebug = false
	if status = m.renderStatus(); strings.Contains(status, "Next heartbeat:") {
		t.Fatalf("heartbeat details leaked outside debug mode: %q", status)
	}
}

func TestHeartbeatImageRequestWithoutVisibleReplyDoesNotCountAsAction(t *testing.T) {
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval}, testMemoryStore(t))
	m.phase = phaseChatting
	updated, _ := m.Update(responseMsg{
		heartbeat: true,
		result: completionResult{
			HadToolCalls: true,
			ImageRequest: &imageRequest{ToolCallID: "image-1", Prompt: "Show me", Reason: "Inspect it"},
		},
	})
	m = updated.(appModel)
	if m.phase != phaseImageRequest {
		t.Fatalf("heartbeat image request phase = %d, want %d", m.phase, phaseImageRequest)
	}

	m.phase = phaseChatting
	updated, _ = m.Update(responseMsg{heartbeat: true, result: completionResult{
		Reply:        "",
		HadToolCalls: true,
	}})
	m = updated.(appModel)
	if !m.heartbeatArmed {
		t.Fatal("silent image-request flow did not schedule another heartbeat")
	}
}

func TestHeartbeatToolRoundWithVisibleReplyCountsAsAction(t *testing.T) {
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval}, testMemoryStore(t))
	m.phase = phaseChatting
	updated, _ := m.Update(responseMsg{heartbeat: true, result: completionResult{
		HadToolCalls:    true,
		HadVisibleReply: true,
	}})
	m = updated.(appModel)
	if m.heartbeatArmed {
		t.Fatal("heartbeat remained armed after visible text in a tool round")
	}
}

func TestHeartbeatImageRequestRetainsVisibleReplyAcrossConsent(t *testing.T) {
	m := newAppModel(appConfig{Heartbeat: defaultHeartbeatInterval, RequestTimeout: time.Second}, testMemoryStore(t))
	m.phase = phaseImageRequest
	m.imageRequest = &imageRequest{ToolCallID: "image-1", Prompt: "Show me", Reason: "Inspect it"}
	m.messages = []apiMessage{{
		Role: "assistant",
		ToolCalls: []toolCall{{
			ID:       "image-1",
			Type:     "function",
			Function: functionCall{Name: "request_image", Arguments: `{}`},
		}},
	}}
	m.requestHeartbeat = true
	m.requestVisibleReply = true
	if cmd := m.finishImageRequest("", true); cmd == nil {
		t.Fatal("declining the image did not continue the completion")
	}
	if !m.streamVisibleReply {
		t.Fatal("visible reply was not carried into the continuation stream")
	}

	session := m.streamSession
	updated, _ := m.Update(completionStreamMsg{
		session: session,
		events:  make(chan completionStreamEvent),
		event: completionStreamEvent{
			Kind:   streamFinished,
			Result: completionResult{Reply: ""},
		},
		ok: true,
	})
	m = updated.(appModel)
	if m.heartbeatArmed {
		t.Fatal("heartbeat re-armed after visible text preceding image consent")
	}
}

func TestScheduleNextHeartbeatToolOnlyWorksForRegularReplies(t *testing.T) {
	client := newAPIClient(modelChoice{}, 16384, testMemoryStore(t), time.Second)
	call := toolCall{Function: functionCall{
		Name:      "schedule_next_heartbeat",
		Arguments: `{"when":"30m"}`,
	}}
	client.heartbeatSchedulingAllowed = true
	result, notice := client.runTool(call)
	if client.nextHeartbeat == nil || !strings.Contains(result, "scheduled_for") || notice == "" {
		t.Fatalf("regular scheduling result = %q, notice = %q, next = %v", result, notice, client.nextHeartbeat)
	}

	client.nextHeartbeat = nil
	client.heartbeatSchedulingAllowed = false
	result, _ = client.runTool(call)
	if client.nextHeartbeat != nil || !strings.Contains(result, "only be scheduled") {
		t.Fatalf("heartbeat scheduling was accepted during a heartbeat: %q", result)
	}
}

func TestHeartbeatRequestDoesNotAdvertiseSchedulingTool(t *testing.T) {
	t.Parallel()
	requests := make(chan chatRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- request
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "heartbeat-none", "type": "function",
				"function": map[string]any{"name": "complete_heartbeat", "arguments": `{"action":"none"}`},
			}},
		}}}})
	}))
	defer server.Close()

	client := newAPIClient(modelChoice{
		Backend: backendLlama,
		Model:   "gemma4-heretical",
		BaseURL: server.URL,
		APIKey:  "none",
	}, 16384, testMemoryStore(t), 5*time.Second)
	for event := range client.completeStream(context.Background(), []apiMessage{
		{Role: "user", Content: "hello"},
		{Role: "system", Content: heartbeatEventPrompt(`{"sent_at":"now"}`)},
	}, false) {
		if event.Kind == streamFinished && event.Err != nil {
			t.Fatal(event.Err)
		}
	}

	request := <-requests
	wantTools := len(client.heartbeatTools())
	if len(request.Tools) != wantTools {
		t.Fatalf("heartbeat advertised %d tools, want %d", len(request.Tools), wantTools)
	}
	for _, tool := range request.Tools {
		if tool.Function.Name == "schedule_next_heartbeat" {
			t.Fatal("heartbeat request advertised schedule_next_heartbeat")
		}
		if tool.Function.Name == "request_image" {
			t.Fatal("heartbeat request advertised request_image")
		}
	}
	if !hasTool(request.Tools, "complete_heartbeat") || request.ToolChoice != "required" {
		t.Fatalf("heartbeat completion was not structurally required: tools=%#v choice=%#v", request.Tools, request.ToolChoice)
	}
}

func TestHeartbeatInstructionsDistinguishToolCallsFromResponseContent(t *testing.T) {
	toolDescription := ""
	for _, tool := range tools {
		if tool.Function.Name == "schedule_next_heartbeat" {
			toolDescription = tool.Function.Description
			break
		}
	}
	if toolDescription == "" {
		t.Fatal("schedule_next_heartbeat tool description is missing")
	}
	for name, guidance := range map[string]string{
		"tool description": toolDescription,
		"system guidance":  memoryToolInstructions,
	} {
		for _, alternatives := range [][]string{
			{"regular user turn"},
			{"structured tool-calling channel", "toolcall"},
			{"assistant response content", "response body"},
			{"default interval"},
		} {
			matched := false
			for _, alternative := range alternatives {
				if strings.Contains(guidance, alternative) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("%s does not contain one of %q: %s", name, alternatives, guidance)
			}
		}
		if strings.Contains(guidance, "alongside your final visible response") {
			t.Errorf("%s still requests mixed text and tool output: %s", name, guidance)
		}
	}
}

func testMemoryStore(t *testing.T) *memoryStore {
	t.Helper()
	dir := t.TempDir()
	return &memoryStore{
		dir:                dir,
		memoriesPath:       filepath.Join(dir, "memories.jsonl"),
		recentMessagesPath: filepath.Join(dir, "recent-messages.jsonl"),
		lastActivity:       filepath.Join(dir, "lastactivity.txt"),
	}
}
