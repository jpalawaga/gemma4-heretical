package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCategories(t *testing.T, store *memoryStore, content string) {
	t.Helper()
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.categoriesPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseMemoryCategoriesNormalizesAndDedupes(t *testing.T) {
	t.Parallel()
	categories := parseMemoryCategories("# comment\nLasts: last time something happened\n\n  Gym : training notes\nlasts: duplicate\nnodesc\n")
	if len(categories) != 3 {
		t.Fatalf("unexpected categories: %#v", categories)
	}
	if categories[0].Name != "lasts" || categories[0].Description != "last time something happened" {
		t.Errorf("first category = %#v", categories[0])
	}
	if categories[1].Name != "gym" || categories[2].Name != "nodesc" || categories[2].Description != "" {
		t.Errorf("categories = %#v", categories)
	}
}

func TestLegacyTaggedMemoriesMapOntoCategories(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	writeCategories(t, store, "lasts: when things last happened\ngym: training\n")
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := strings.Join([]string{
		`{"timestamp":"2026-01-01 10:00:00","content":"last haircut in March","tags":["userinfo","lasts"]}`,
		`{"timestamp":"2026-01-01 10:01:00","content":"deadlift 140","tags":["Gym"]}`,
		`{"timestamp":"2026-01-01 10:02:00","content":"no tags at all"}`,
		`{"timestamp":"2026-01-01 10:03:00","content":"explicit wins","category":"gym","tags":["lasts"]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(store.memoriesPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, entry := range entries {
		got[entry.Content] = entry.Category
	}
	want := map[string]string{
		"last haircut in March": "lasts",   // configured tag wins over an unconfigured first tag
		"deadlift 140":          "gym",     // normalized
		"no tags at all":        "general", // default
		"explicit wins":         "gym",     // explicit category beats tags
	}
	for content, category := range want {
		if got[content] != category {
			t.Errorf("%q filed under %q, want %q", content, got[content], category)
		}
	}
	if result := store.List("lasts"); !strings.Contains(result, "last haircut in March") || strings.Contains(result, "deadlift") {
		t.Errorf("category listing wrong: %q", result)
	}
}

func TestConfiguredCategoriesAreEnforcedAndListed(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	writeCategories(t, store, "lasts: when things last happened\ngym: training notes\n")
	if _, err := store.Save("squat 100", "Gym"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save("something", "random"); err == nil || !strings.Contains(err.Error(), "lasts, gym") {
		t.Fatalf("unknown category should be rejected with the allowed list, got %v", err)
	}
	if _, err := store.Save("something", ""); err == nil {
		t.Fatal("empty category must not fall back to general when categories are configured")
	}
	summary := store.List("")
	if !strings.Contains(summary, "- lasts (0): when things last happened") || !strings.Contains(summary, "- gym (1): training notes") {
		t.Fatalf("category summary wrong: %q", summary)
	}

	initial, _, count, err := store.InitialSystemMessages("")
	if err != nil || count != 1 {
		t.Fatalf("startup failed: count=%d err=%v", count, err)
	}
	encoded, _ := json.Marshal(initial)
	for _, expected := range []string{"## gym", "- squat 100", "Memory categories are configured", "- lasts: when things last happened", "list_memories", "move_memory"} {
		if !strings.Contains(string(encoded), expected) {
			t.Errorf("startup context omitted %q", expected)
		}
	}
	if strings.Contains(string(encoded), "search_memories_by_tag") {
		t.Error("startup context still advertises the removed tag search tool")
	}
}

func TestFreeFormCategoriesWhenUnconfigured(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	if _, err := store.Save("anything goes", "Pet Peeves"); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Save("defaulted", "")
	if err != nil || entry.Category != defaultMemoryCategory {
		t.Fatalf("empty category should default when unconfigured: %#v %v", entry, err)
	}
	if result := store.List(""); !strings.Contains(result, "- pet-peeves (1)") || !strings.Contains(result, "- general (1)") {
		t.Fatalf("summary wrong: %q", result)
	}
}

func TestMoveAndScopedForget(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	for _, pair := range [][2]string{{"ran 5k", "general"}, {"ran 10k", "general"}, {"ran out of milk", "errands"}} {
		if _, err := store.Save(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	moved, category, err := store.Move("ran", "gym")
	if err != nil || moved != 3 || category != "gym" {
		t.Fatalf("move = %d %q %v", moved, category, err)
	}
	moved, _, err = store.Move("ran", "gym")
	if err != nil || moved != 0 {
		t.Fatalf("second move should be a no-op: %d %v", moved, err)
	}
	removed, err := store.Forget("ran", "errands")
	if err != nil || removed != 0 {
		t.Fatalf("scoped forget touched the wrong category: %d %v", removed, err)
	}
	removed, err = store.Forget("milk", "gym")
	if err != nil || removed != 1 {
		t.Fatalf("scoped forget = %d %v", removed, err)
	}
	if result := store.Search("ran", "gym"); !strings.Contains(result, "ran 5k") || strings.Contains(result, "milk") {
		t.Fatalf("scoped search wrong: %q", result)
	}
}

func TestMemoryToolsCarryCategoryEnumAndRunToolFilesByCategory(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	writeCategories(t, store, "lasts\ngym\n")
	client := newAPIClient(modelChoice{Backend: backendLlama, Model: "m", BaseURL: "http://127.0.0.1:1", APIKey: "none"}, 4096, store, time.Second)
	if _, _, err := client.EnableCapability("memory-admin"); err != nil {
		t.Fatal(err)
	}

	var saveSpec, listSpec *toolSpec
	for index := range tools {
		switch tools[index].Function.Name {
		case "save_memory":
			saveSpec = &tools[index]
		case "search_memories_by_tag":
			t.Fatal("tag search tool should be gone")
		}
	}
	for _, tool := range client.completionTools(true) {
		tool := tool
		switch tool.Function.Name {
		case "save_memory":
			properties := tool.Function.Parameters["properties"].(map[string]any)
			enum, _ := properties["category"].(map[string]any)["enum"].([]string)
			if strings.Join(enum, ",") != "lasts,gym" {
				t.Fatalf("save_memory category enum = %#v", enum)
			}
		case "list_memories":
			listSpec = &tool
		}
	}
	if listSpec == nil {
		t.Fatal("list_memories tool missing")
	}
	if _, has := saveSpec.Function.Parameters["properties"].(map[string]any)["category"].(map[string]any)["enum"]; has {
		t.Fatal("package-level tool spec was mutated with an enum")
	}

	result, notice := client.runTool(toolCall{ID: "1", Function: functionCall{Name: "save_memory", Arguments: `{"content":"bench 80","category":"gym"}`}})
	if notice != "memory saved" || !strings.Contains(result, "Saved in gym: bench 80") {
		t.Fatalf("save result = %q notice=%q", result, notice)
	}
	result, _ = client.runTool(toolCall{ID: "2", Function: functionCall{Name: "save_memory", Arguments: `{"content":"x","category":"nope"}`}})
	if !strings.Contains(result, "Could not save memory") || !strings.Contains(result, "lasts, gym") {
		t.Fatalf("bad category result = %q", result)
	}
	result, _ = client.runTool(toolCall{ID: "3", Function: functionCall{Name: "list_memories", Arguments: `{"category":"gym"}`}})
	if !strings.Contains(result, "[gym] bench 80") {
		t.Fatalf("list result = %q", result)
	}
	result, notice = client.runTool(toolCall{ID: "4", Function: functionCall{Name: "move_memory", Arguments: `{"query":"bench","category":"lasts"}`}})
	if notice != "memory updated" || !strings.Contains(result, "Moved 1 memory entry(s) to lasts") {
		t.Fatalf("move result = %q notice=%q", result, notice)
	}
	result, notice = client.runTool(toolCall{ID: "5", Function: functionCall{Name: "update_memory", Arguments: `{"query":"bench","content":"bench 85 for 5","category":"gym"}`}})
	if notice != "memory updated" || !strings.Contains(result, "Updated in gym: bench 85 for 5") {
		t.Fatalf("update result = %q notice=%q", result, notice)
	}
	if result, _ = client.runTool(toolCall{ID: "6", Function: functionCall{Name: "list_memories", Arguments: `{"category":"gym"}`}}); !strings.Contains(result, "[gym] bench 85 for 5") || strings.Contains(result, "bench 80") {
		t.Fatalf("update did not replace the entry: %q", result)
	}
}

func TestUpdateMemoryRequiresASingleMatch(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	for _, content := range []string{"ran 5k on Monday", "ran 10k on Sunday", "cooked dinner"} {
		if _, err := store.Save(content, "log"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Update("ran", "x", ""); err == nil || !strings.Contains(err.Error(), "2 memories match") || !strings.Contains(err.Error(), "ran 10k on Sunday") {
		t.Fatalf("ambiguous update should list candidates, got %v", err)
	}
	if _, err := store.Update("swam", "x", ""); err == nil || !strings.Contains(err.Error(), "no memory matches") {
		t.Fatalf("missing match should error, got %v", err)
	}
	entry, err := store.Update("10k", "ran 10k on Sunday in 52:10", "")
	if err != nil || entry.Content != "ran 10k on Sunday in 52:10" || entry.Category != "log" {
		t.Fatalf("update = %#v %v", entry, err)
	}
	if entries, _ := store.Load(); len(entries) != 3 {
		t.Fatalf("update changed the entry count: %d", len(entries))
	}
}

func TestMemoryCommandListsCategoriesInTranscript(t *testing.T) {
	store := testMemoryStore(t)
	writeCategories(t, store, "lasts: when things last happened\n")
	if _, err := store.Save("last dentist visit in May", "lasts"); err != nil {
		t.Fatal(err)
	}
	m := newAppModel(appConfig{}, store)
	m.phase = phaseChatting
	m.width, m.height = 100, 40
	m.resize()
	for _, tc := range []struct{ command, expect string }{
		{"/memory list lasts", "[lasts] last dentist visit in May"},
		{"/memory", "- lasts (1): when things last happened"},
		{"/memory categories", filepath.Base(store.categoriesPath())},
		{"/memory search dentist", "last dentist visit"},
	} {
		handled, _ := m.handleCommand(tc.command)
		if !handled {
			t.Fatalf("%q was not handled", tc.command)
		}
		last := m.transcript[len(m.transcript)-1]
		if last.Role != "notice" || !strings.Contains(last.Content, tc.expect) {
			t.Fatalf("%q produced %#v, want %q", tc.command, last, tc.expect)
		}
	}
	if handled, _ := m.handleCommand("/memory bogus"); !handled || !strings.HasPrefix(m.status, "Usage:") {
		t.Fatalf("bad subcommand should show usage, status=%q", m.status)
	}
}
