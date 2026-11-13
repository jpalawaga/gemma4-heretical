package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestSettingsRoundTripAndValidation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings.json")
	loaded, found, err := loadSettings(path)
	if err != nil || found || loaded != defaultSettings() {
		t.Fatalf("missing file should yield defaults: found=%v err=%v %#v", found, err, loaded)
	}
	value := defaultSettings()
	value.PreferredModel = "llama.cpp/qwen3.8-27b-heretical"
	value.Context = 65536
	value.Heartbeat = "0"
	value.Debug = true
	if err := saveSettings(path, value); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("settings file is not JSON: %v\n%s", err, raw)
	}
	if decoded["preferred_model"] != "llama.cpp/qwen3.8-27b-heretical" || decoded["context"] != float64(65536) || decoded["heartbeat"] != "0" {
		t.Fatalf("unexpected JSON: %s", raw)
	}
	reloaded, found, err := loadSettings(path)
	if err != nil || !found || reloaded != value {
		t.Fatalf("round trip mismatch: found=%v err=%v %#v", found, err, reloaded)
	}
	if reloaded.heartbeat() != 0 || reloaded.requestTimeout() != 10*time.Minute {
		t.Fatalf("duration accessors wrong: %v %v", reloaded.heartbeat(), reloaded.requestTimeout())
	}

	bad := defaultSettings()
	bad.RequestTimeout = "soon"
	if err := saveSettings(path, bad); err == nil {
		t.Fatal("invalid duration should not be saved")
	}
	if err := os.WriteFile(path, []byte(`{"context": -5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadSettings(path); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("invalid stored settings should surface an error, got %v", err)
	}
}

func TestParseConfigLayersSettingsEnvironmentAndFlags(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings.json")
	stored := defaultSettings()
	stored.Context = 32768
	stored.SystemFile = "memory.md"
	stored.Heartbeat = "0"
	stored.LeanTools = true
	stored.PreferredModel = "ollama/gemma4-heretical"
	if err := saveSettings(path, stored); err != nil {
		t.Fatal(err)
	}
	noEnv := func(string) string { return "" }

	cfg, err := parseConfigFrom(nil, noEnv, path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context != 32768 || cfg.SystemFile != "memory.md" || cfg.Heartbeat != 0 || !cfg.LeanTools || cfg.PreferredModel != "ollama/gemma4-heretical" || cfg.SettingsPath != path {
		t.Fatalf("settings file was not applied: %#v", cfg)
	}

	env := func(name string) string {
		if name == "GEMMA4_CONTEXT" {
			return "65536"
		}
		return ""
	}
	cfg, err = parseConfigFrom(nil, env, path)
	if err != nil || cfg.Context != 65536 {
		t.Fatalf("environment should override the file: %d %v", cfg.Context, err)
	}

	cfg, err = parseConfigFrom([]string{"--context", "1024", "--heartbeat", "5m", "--lean-tools=false", "other.md"}, env, path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context != 1024 || cfg.Heartbeat != 5*time.Minute || cfg.LeanTools || cfg.SystemFile != "other.md" {
		t.Fatalf("flags should override everything: %#v", cfg)
	}
	if cfg.Settings.Context != 1024 || cfg.Settings.Heartbeat != "5m0s" {
		t.Fatalf("effective settings should mirror the config: %#v", cfg.Settings)
	}

	if _, err := parseConfigFrom(nil, func(name string) string { return "lots" }, path); err == nil {
		t.Fatal("non-numeric GEMMA4_CONTEXT should be rejected")
	}
}

func TestSettingsScreenEditsAndSaves(t *testing.T) {
	store := testMemoryStore(t)
	path := filepath.Join(store.dir, "settings.json")
	cfg := defaultSettings().toConfig(appConfig{SettingsPath: path})
	m := newAppModel(cfg, store)
	m.phase = phaseChatting
	m.width, m.height = 160, 40
	m.resize()
	m.choices = []modelChoice{
		{Backend: backendLlama, Model: "qwen3.8-27b-heretical", Online: true},
		{Backend: backendOllama, Model: "gemma4-heretical", Online: false},
	}
	m.client = newAPIClient(modelChoice{}, cfg.Context, store, cfg.RequestTimeout)
	m.client.exposeAllTools = true

	handled, _ := m.handleCommand("/settings")
	if !handled || m.phase != phaseSettings || m.settingsReturn != phaseChatting {
		t.Fatalf("/settings did not open the screen: phase=%v", m.phase)
	}
	view := m.View().Content
	for _, expected := range []string{"Settings", path, "Preferred model", "Ask at startup", "Heartbeat interval", "● off"} {
		if !strings.Contains(view, expected) {
			t.Errorf("settings view missing %q", expected)
		}
	}

	press := func(key string) {
		t.Helper()
		var msg tea.Msg
		switch key {
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		default:
			msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		updated, _ := m.Update(msg)
		m = updated.(appModel)
	}
	fieldIndex := func(key string) int {
		for index, field := range m.settingsFields {
			if field.Key == key {
				return index
			}
		}
		t.Fatalf("no field %s", key)
		return -1
	}

	// Enum: cycle the preferred model to the first discovered choice.
	press("right")
	if m.settingsDraft.PreferredModel != "llama.cpp/qwen3.8-27b-heretical" {
		t.Fatalf("enum did not cycle: %q", m.settingsDraft.PreferredModel)
	}
	// Int: edit the context window inline, including a rejected value.
	for m.settingsCursor < fieldIndex("context") {
		press("down")
	}
	press("enter")
	if !m.settingsEditing {
		t.Fatal("enter on a number row should start editing")
	}
	m.settingsInput.SetValue("zero")
	press("enter")
	if !m.settingsEditing || m.settingsError == "" {
		t.Fatalf("invalid number should stay in edit mode with an error: editing=%v err=%q", m.settingsEditing, m.settingsError)
	}
	m.settingsInput.SetValue("65536")
	press("enter")
	if m.settingsEditing || m.settingsDraft.Context != 65536 {
		t.Fatalf("valid number was not committed: editing=%v ctx=%d", m.settingsEditing, m.settingsDraft.Context)
	}
	// Duration: heartbeat to 0 through the inline editor.
	for m.settingsCursor < fieldIndex("heartbeat") {
		press("down")
	}
	press("enter")
	m.settingsInput.SetValue("0")
	press("enter")
	if m.settingsDraft.Heartbeat != "0" {
		t.Fatalf("heartbeat = %q", m.settingsDraft.Heartbeat)
	}
	// Bool: toggle debug with Enter.
	for m.settingsCursor < fieldIndex("debug") {
		press("down")
	}
	press("enter")
	if !m.settingsDraft.Debug {
		t.Fatal("enter did not toggle the debug switch")
	}

	// Nothing is applied or written until save.
	if m.showDebug || m.cfg.Context != 16384 {
		t.Fatal("draft leaked into the live config before saving")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("settings file written before save")
	}
	press("s")
	if m.phase != phaseChatting {
		t.Fatalf("save should return to chat, phase=%v", m.phase)
	}
	if !m.showDebug || m.cfg.Context != 65536 || m.cfg.Heartbeat != 0 || m.cfg.PreferredModel != "llama.cpp/qwen3.8-27b-heretical" || m.client.context != 65536 {
		t.Fatalf("saved settings were not applied live: %#v showDebug=%v client.ctx=%d", m.cfg, m.showDebug, m.client.context)
	}
	saved, found, err := loadSettings(path)
	if err != nil || !found || saved.Context != 65536 || !saved.Debug || saved.Heartbeat != "0" {
		t.Fatalf("settings file mismatch: found=%v err=%v %#v", found, err, saved)
	}

	// Esc discards a draft.
	m.openSettings()
	press("right")
	if m.settingsDraft.PreferredModel == m.cfg.PreferredModel {
		t.Fatal("draft should have moved to the next model option")
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(appModel)
	if m.phase != phaseChatting || m.cfg.PreferredModel != "llama.cpp/qwen3.8-27b-heretical" {
		t.Fatalf("esc should discard the draft: phase=%v preferred=%q", m.phase, m.cfg.PreferredModel)
	}
	saved, _, _ = loadSettings(path)
	if saved.PreferredModel != "llama.cpp/qwen3.8-27b-heretical" {
		t.Fatalf("esc must not write the file: %#v", saved)
	}
}

func TestPreferredModelAutoConnectsOnceWhenOnline(t *testing.T) {
	store := testMemoryStore(t)
	cfg := defaultSettings().toConfig(appConfig{})
	cfg.PreferredModel = "llama.cpp/qwen3.8-27b-heretical"
	m := newAppModel(cfg, store)
	choices := []modelChoice{
		{Backend: backendOllama, Model: "gemma4-heretical", Online: true},
		{Backend: backendLlama, Model: "qwen3.8-27b-heretical", Online: true, BaseURL: "http://127.0.0.1:1", APIKey: "none"},
	}
	updated, cmd := m.Update(discoveredMsg{choices: choices})
	m = updated.(appModel)
	if m.phase != phaseStarting || m.selected.Model != "qwen3.8-27b-heretical" || cmd == nil {
		t.Fatalf("preferred model was not auto-selected: phase=%v selected=%#v", m.phase, m.selected)
	}
	m.stopStartup()

	// A later rediscovery (Ctrl+G, r) must not hijack the picker again.
	m.phase = phaseDiscovering
	updated, _ = m.Update(discoveredMsg{choices: choices})
	m = updated.(appModel)
	if m.phase != phasePicking {
		t.Fatalf("second discovery should stay on the picker, phase=%v", m.phase)
	}

	offline := newAppModel(cfg, store)
	updated, _ = offline.Update(discoveredMsg{choices: []modelChoice{{Backend: backendLlama, Model: "qwen3.8-27b-heretical", Online: false}}})
	offline = updated.(appModel)
	if offline.phase != phasePicking || !strings.Contains(offline.status, "offline") {
		t.Fatalf("offline preferred model should fall back to the picker: phase=%v status=%q", offline.phase, offline.status)
	}
}

func TestMemoryCategoriesFileSettingOverridesLocation(t *testing.T) {
	t.Parallel()
	store := testMemoryStore(t)
	custom := filepath.Join(t.TempDir(), "cats.txt")
	if err := os.WriteFile(custom, []byte("lasts: when\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if categories, _ := store.Categories(); len(categories) != 0 {
		t.Fatalf("default location should be empty here: %#v", categories)
	}
	store.categoriesFile = custom
	categories, err := store.Categories()
	if err != nil || len(categories) != 1 || categories[0].Name != "lasts" {
		t.Fatalf("override not honoured: %#v %v", categories, err)
	}
	if _, err := store.Save("x", "gym"); err == nil {
		t.Fatal("categories from the override file should be enforced")
	}

	path := filepath.Join(t.TempDir(), "settings.json")
	value := defaultSettings()
	value.MemoryCategoriesFile = custom
	if err := saveSettings(path, value); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfigFrom(nil, func(string) string { return "" }, path)
	if err != nil || cfg.MemoryCategoriesFile != custom {
		t.Fatalf("setting did not reach the config: %q %v", cfg.MemoryCategoriesFile, err)
	}
	cfg, err = parseConfigFrom(nil, func(name string) string {
		if name == "GEMMA4_MEMORY_CATEGORIES" {
			return "/elsewhere/cats.txt"
		}
		return ""
	}, path)
	if err != nil || cfg.MemoryCategoriesFile != "/elsewhere/cats.txt" {
		t.Fatalf("environment override failed: %q %v", cfg.MemoryCategoriesFile, err)
	}
}

func TestCategoryEditorFromSettingsWritesCategoriesFile(t *testing.T) {
	store := testMemoryStore(t)
	writeCategories(t, store, "lasts: when things last happened\ngym: training\n")
	if _, err := store.Save("deadlift 140", "gym"); err != nil {
		t.Fatal(err)
	}
	cfg := defaultSettings().toConfig(appConfig{SettingsPath: filepath.Join(store.dir, "settings.json")})
	m := newAppModel(cfg, store)
	m.phase = phaseChatting
	m.width, m.height = 160, 40
	m.resize()
	m.openSettings()

	press := func(key string) {
		t.Helper()
		var msg tea.Msg
		switch key {
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "shift+up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		default:
			msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		updated, _ := m.Update(msg)
		m = updated.(appModel)
	}
	for m.settingsFields[m.settingsCursor].Key != "memory_categories" {
		press("down")
	}
	if !strings.Contains(m.View().Content, "2 configured") {
		t.Fatalf("settings row should show the configured count: %s", m.View().Content)
	}
	press("enter")
	if m.phase != phaseCategoryEditor || len(m.categoriesDraft) != 2 {
		t.Fatalf("category editor did not open: phase=%v draft=%#v", m.phase, m.categoriesDraft)
	}
	if view := m.View().Content; !strings.Contains(view, "lasts") || !strings.Contains(view, "(1)") || !strings.Contains(view, "training") {
		t.Fatalf("editor view wrong: %s", view)
	}

	// Add, with a duplicate rejected first.
	press("a")
	m.categoriesInput.SetValue("Gym: duplicate")
	press("enter")
	if !m.categoriesEditing || !strings.Contains(m.categoriesError, "already exists") {
		t.Fatalf("duplicate should be rejected: editing=%v err=%q", m.categoriesEditing, m.categoriesError)
	}
	m.categoriesInput.SetValue("Tasks : things to do")
	press("enter")
	if len(m.categoriesDraft) != 3 || m.categoriesDraft[2].Name != "tasks" || m.categoriesDraft[2].Description != "things to do" || m.categoriesCursor != 2 {
		t.Fatalf("add failed: %#v cursor=%d", m.categoriesDraft, m.categoriesCursor)
	}
	// Reorder the new row to the top, then edit the description of the second row.
	press("shift+up")
	press("shift+up")
	if m.categoriesDraft[0].Name != "tasks" || m.categoriesCursor != 0 {
		t.Fatalf("reorder failed: %#v", m.categoriesDraft)
	}
	press("down")
	press("enter")
	m.categoriesInput.SetValue("lasts: last time something happened")
	press("enter")
	if m.categoriesDraft[1].Description != "last time something happened" {
		t.Fatalf("edit failed: %#v", m.categoriesDraft)
	}
	// Delete gym; its memory should then show as unconfigured.
	press("down")
	press("d")
	if len(m.categoriesDraft) != 2 || m.categoriesCursor != 1 {
		t.Fatalf("delete failed: %#v cursor=%d", m.categoriesDraft, m.categoriesCursor)
	}
	if !strings.Contains(m.View().Content, "In use but not configured: gym (1)") {
		t.Fatalf("unconfigured warning missing: %s", m.View().Content)
	}

	// Nothing written yet; save writes the file in order and returns to settings.
	if categories, _ := store.Categories(); len(categories) != 2 || categories[0].Name != "lasts" {
		t.Fatalf("file changed before save: %#v", categories)
	}
	press("s")
	if m.phase != phaseSettings || !strings.Contains(m.status, "Saved 2 memory categories") {
		t.Fatalf("save did not return to settings: phase=%v status=%q", m.phase, m.status)
	}
	categories, err := store.Categories()
	if err != nil || len(categories) != 2 || categories[0].Name != "tasks" || categories[1].Description != "last time something happened" {
		t.Fatalf("categories file wrong after save: %#v %v", categories, err)
	}
	raw, _ := os.ReadFile(store.categoriesPath())
	if !strings.HasPrefix(string(raw), "# Memory categories") || !strings.Contains(string(raw), "tasks: things to do\n") {
		t.Fatalf("file format wrong:\n%s", raw)
	}
	if _, err := store.Save("x", "gym"); err == nil {
		t.Fatal("removed category should no longer be accepted")
	}

	// Esc discards edits; an emptied list removes the file.
	if m.settingsFields[m.settingsCursor].Key != "memory_categories" {
		t.Fatalf("cursor should still be on the categories row, got %s", m.settingsFields[m.settingsCursor].Key)
	}
	press("enter")
	press("d")
	press("esc")
	if categories, _ := store.Categories(); len(categories) != 2 {
		t.Fatalf("esc should not change the file: %#v", categories)
	}
	press("enter")
	press("d")
	press("d")
	press("s")
	if _, err := os.Stat(store.categoriesPath()); !os.IsNotExist(err) {
		t.Fatalf("saving an empty list should remove the file, stat err=%v", err)
	}
	if categories, _ := store.Categories(); len(categories) != 0 {
		t.Fatalf("categories should be free-form now: %#v", categories)
	}
}

func TestInlineCompactionInstructionsAreMultilineAndMergedWithFile(t *testing.T) {
	store := testMemoryStore(t)
	cfg := defaultSettings().toConfig(appConfig{SettingsPath: filepath.Join(store.dir, "settings.json")})
	m := newAppModel(cfg, store)
	m.phase = phaseChatting
	m.width, m.height = 160, 40
	m.resize()
	m.openSettings()
	for m.settingsFields[m.settingsCursor].Key != "compaction_instructions" {
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(appModel)
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(appModel)
	if !m.settingsEditing || !strings.Contains(m.View().Content, "Ctrl+S apply") {
		t.Fatalf("multiline editor did not open: editing=%v", m.settingsEditing)
	}
	m.settingsArea.SetValue("Note my mood.")
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(appModel)
	if !m.settingsEditing || !strings.Contains(m.settingsArea.Value(), "\n") {
		t.Fatalf("enter should insert a newline in the multiline editor: editing=%v value=%q", m.settingsEditing, m.settingsArea.Value())
	}
	m.settingsArea.InsertString("Skip debugging tangents.")
	updated, _ = m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = updated.(appModel)
	if m.settingsEditing || m.settingsDraft.CompactionInstructions != "Note my mood.\nSkip debugging tangents." {
		t.Fatalf("ctrl+s did not commit: editing=%v value=%q", m.settingsEditing, m.settingsDraft.CompactionInstructions)
	}
	if !strings.Contains(m.View().Content, "Note my mood.  … (+1 line)") {
		t.Fatalf("multiline preview wrong: %s", m.View().Content)
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = updated.(appModel)
	if m.cfg.CompactionInstructions != "Note my mood.\nSkip debugging tangents." {
		t.Fatalf("saved instructions not applied: %q", m.cfg.CompactionInstructions)
	}

	// Both inline and file instructions reach the compaction request.
	file := filepath.Join(t.TempDir(), "more.md")
	if err := os.WriteFile(file, []byte("Keep it under 150 words.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.CompactionInstructionsFile = file
	var saw chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&saw)
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"role":       "assistant",
			"tool_calls": []any{map[string]any{"index": 0, "id": "c-1", "type": "function", "function": map[string]any{"name": "complete_compaction", "arguments": `{"summary":"ok"}`}}},
		}}}})
	}))
	defer server.Close()
	m.client = newAPIClient(modelChoice{Backend: backendLlama, Model: "test", BaseURL: server.URL, APIKey: "none"}, 16384, store, 5*time.Second)
	m.messages = []apiMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
	m.sessionReplies = 1
	m.phase = phaseChatting
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
	last, _ := saw.Messages[len(saw.Messages)-1].Content.(string)
	if !strings.Contains(last, "Note my mood.\nSkip debugging tangents.\n\nKeep it under 150 words.") {
		t.Fatalf("instructions were not merged into the request:\n%s", last)
	}
}
