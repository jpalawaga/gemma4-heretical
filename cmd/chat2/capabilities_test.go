package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRegularTurnsStartLeanAndRequestToolcallExposesGroups(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	client := newAPIClient(modelChoice{Backend: backendLlama, Model: "m", BaseURL: "http://127.0.0.1:1", APIKey: "none"}, 4096, store, time.Second)

	initial := client.completionTools(true)
	for _, core := range []string{"save_memory", "search_memories", "get_current_time", "request_toolcall", "request_tool", "schedule_next_heartbeat"} {
		if !hasTool(initial, core) {
			t.Errorf("core tool %s missing from a lean turn", core)
		}
	}
	for _, gated := range []string{"schedule_item", "list_scheduled_items", "complete_scheduled_item", "list_memories", "move_memory", "forget_memory", "request_image"} {
		if hasTool(initial, gated) {
			t.Errorf("gated tool %s exposed before being requested", gated)
		}
	}
	// Heartbeats are private maintenance turns and see every gated tool.
	heartbeat := client.heartbeatTools()
	for _, name := range []string{"schedule_item", "list_memories", "complete_heartbeat"} {
		if !hasTool(heartbeat, name) {
			t.Errorf("heartbeat missing %s", name)
		}
	}
	for _, name := range []string{"request_toolcall", "request_tool", "request_image"} {
		if hasTool(heartbeat, name) {
			t.Errorf("heartbeat should not offer %s", name)
		}
	}

	result, notice := client.runTool(toolCall{ID: "1", Function: functionCall{Name: "request_toolcall", Arguments: `{"capability":"scheduling","reason":"the user asked for a reminder"}`}})
	if notice != "enabled capability scheduling" {
		t.Fatalf("notice = %q, result = %s", notice, result)
	}
	var decoded struct {
		Status     string   `json:"status"`
		Capability string   `json:"capability"`
		Tools      []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil || decoded.Status != "enabled" || decoded.Capability != "scheduling" || len(decoded.Tools) != 3 {
		t.Fatalf("unexpected request_toolcall result: %s (%v)", result, err)
	}
	after := client.completionTools(true)
	if !hasTool(after, "schedule_item") || hasTool(after, "list_memories") {
		t.Fatalf("scheduling should now be exposed and memory-admin still hidden: %#v", toolNames(after))
	}

	result, notice = client.runTool(toolCall{ID: "2", Function: functionCall{Name: "request_toolcall", Arguments: `{"capability":"scheduling","reason":"again"}`}})
	if notice != "" || !strings.Contains(result, "already_enabled") {
		t.Fatalf("second request should be a silent no-op: %q %s", notice, result)
	}
	result, notice = client.runTool(toolCall{ID: "3", Function: functionCall{Name: "request_toolcall", Arguments: `{"capability":"teleport","reason":"x"}`}})
	if notice != "" || !strings.Contains(result, "unknown capability") || !strings.Contains(result, "scheduling, memory-admin, images, whoop") {
		t.Fatalf("unknown capability should list the catalogue: %s", result)
	}
	result, _ = client.runTool(toolCall{ID: "4", Function: functionCall{Name: "request_toolcall", Arguments: `{"capability":"images"}`}})
	if !strings.Contains(result, "reason is required") {
		t.Fatalf("missing reason should be rejected: %s", result)
	}

	chips, remaining := mutationNoticeChips([]string{"enabled capability scheduling"})
	if len(chips) != 1 || chips[0].Kind != "tools" || chips[0].Label != "Tools: scheduling" || len(remaining) != 0 {
		t.Fatalf("capability notice did not render as a chip: %#v %#v", chips, remaining)
	}
}

func TestDefaultExposesEverythingAndLeanModeAdvertisesCapabilities(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	client := newAPIClient(modelChoice{}, 4096, store, time.Second)
	client.exposeAllTools = true
	if !hasTool(client.completionTools(true), "schedule_item") || !hasTool(client.completionTools(true), "update_memory") {
		t.Fatal("the default should expose gated groups immediately")
	}
	if len(client.EnabledCapabilities()) != len(toolCapabilities) {
		t.Fatalf("enabled capabilities = %v", client.EnabledCapabilities())
	}

	// Ordinary startup context does not talk about hidden groups.
	messages, _, _, err := store.InitialSystemMessages("")
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(messages); strings.Contains(string(encoded), "Hidden tool groups") {
		t.Error("default startup context should not describe tool gating")
	}

	lean := leanToolsInstructions(false)
	for _, expected := range []string{"request_toolcall", "- scheduling:", "- memory-admin:", "- images:"} {
		if !strings.Contains(lean, expected) {
			t.Errorf("lean instructions omitted %q", expected)
		}
	}
	if strings.Contains(lean, "- whoop:") || !strings.Contains(leanToolsInstructions(true), "- whoop:") {
		t.Error("Whoop capability should be advertised only once connected")
	}
}

func TestToolsCommandTogglesCapabilities(t *testing.T) {
	store := testMemoryStore(t)
	m := newAppModel(appConfig{}, store)
	m.phase = phaseChatting
	m.width, m.height = 100, 40
	m.resize()
	if handled, _ := m.handleCommand("/tools"); !handled || m.status != "Choose a model first" {
		t.Fatalf("/tools without a client should explain itself, status=%q", m.status)
	}
	m.client = newAPIClient(modelChoice{}, 4096, store, time.Second)
	if handled, _ := m.handleCommand("/tools enable scheduling"); !handled || !m.client.capabilityEnabled("scheduling") {
		t.Fatalf("enable failed: status=%q", m.status)
	}
	last := m.transcript[len(m.transcript)-1]
	if last.Role != "notice" || !strings.Contains(last.Content, "scheduling (enabled)") || !strings.Contains(last.Content, "memory-admin (hidden)") {
		t.Fatalf("tools notice wrong: %#v", last)
	}
	if handled, _ := m.handleCommand("/tools disable scheduling"); !handled || m.client.capabilityEnabled("scheduling") {
		t.Fatalf("disable failed: status=%q", m.status)
	}
	if handled, _ := m.handleCommand("/tools enable nothing"); !handled || !strings.Contains(m.status, "unknown capability") {
		t.Fatalf("bad capability should report, status=%q", m.status)
	}
}

func toolNames(specs []toolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Function.Name)
	}
	return names
}

func TestScheduleManagerWrapsLongItems(t *testing.T) {
	m := newAppModel(appConfig{}, testMemoryStore(t))
	m.phase = phaseScheduleManager
	m.width, m.height = 90, 30
	m.scheduleManagerSelected = map[int]struct{}{}
	m.status = "Ready"
	long := "Call the dentist to reschedule the cleaning that was moved from last month, ask about the night guard quote, and confirm whether the insurance pre-authorisation came through"
	done := time.Now().Add(-time.Hour)
	m.scheduleManagerEntries = []scheduleEntry{
		{ID: 1, Time: time.Now().Add(time.Hour), Event: long},
		{ID: 2, Time: done, Event: "Short one", CompletedAt: &done, CompletedBy: "user", CompletionComment: "Done after the gym, took longer than expected because the receptionist had to check two systems"},
	}
	view := strings.ReplaceAll(m.View().Content, "\n", " ")
	for _, fragment := range []string{"Call the dentist", "insurance pre-authorisation came through", "Short one", "took longer than expected", "two systems"} {
		if !strings.Contains(view, fragment) {
			t.Errorf("schedule view lost %q", fragment)
		}
	}
	if strings.Contains(m.View().Content, "…") {
		t.Fatalf("long items should wrap rather than truncate:\n%s", m.View().Content)
	}
	// Paging keeps the cursor entry visible when the terminal is short.
	m.height = 18
	m.scheduleManagerCursor = 1
	if view := m.View().Content; !strings.Contains(view, "› [✓] #2") || !strings.Contains(view, "showing 2–2 of 2") {
		t.Fatalf("cursor entry should be visible with paging:\n%s", view)
	}
}
