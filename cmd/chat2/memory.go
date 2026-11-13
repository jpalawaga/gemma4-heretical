package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type memoryStore struct {
	dir                string
	memoriesPath       string
	recentMessagesPath string
	lastActivity       string
	// categoriesFile overrides the default categories file location when set
	// (the memory_categories_file setting).
	categoriesFile string
	mu             sync.Mutex
}

const recentMessageLimit = 5

// memoryCategoriesFile lists the categories the agent may file memories under,
// one per line as "name: description". Blank lines and # comments are
// ignored. When the file is absent, categories are free-form.
const memoryCategoriesFile = "memory-categories.txt"

// defaultMemoryCategory is used for entries that carry no category and no
// legacy tags.
const defaultMemoryCategory = "general"

type memoryCategory struct {
	Name        string
	Description string
}

// compactedConversationFile holds the model-written handoff summaries of the
// most recent sessions, newest last, each under a datestamped heading. They
// are written when the user exits and loaded at the next startup.
const compactedConversationFile = "last-conversation-compacted.txt"

// compactedConversationHeader is the pre-history single-entry format.
const compactedConversationHeader = "Compacted at "

// compactedSessionHeader introduces each entry in the history format.
const compactedSessionHeader = "## Session compacted "

const defaultCompactionHistory = 5

type compactedConversation struct {
	At      time.Time
	Summary string
}

func newMemoryStore() (*memoryStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".gemma4-heretical")
	return &memoryStore{
		dir:                dir,
		memoriesPath:       filepath.Join(dir, "memories.jsonl"),
		recentMessagesPath: filepath.Join(dir, "recent-messages.jsonl"),
		lastActivity:       filepath.Join(dir, "lastactivity.txt"),
	}, nil
}

func (s *memoryStore) Load() ([]memoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadUnlocked()
}

func (s *memoryStore) loadUnlocked() ([]memoryEntry, error) {
	file, err := os.Open(s.memoriesPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var result []memoryEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		var entry memoryEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			result = append(result, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	categories, err := s.Categories()
	if err != nil {
		return nil, err
	}
	for index := range result {
		result[index].Category = effectiveCategory(result[index], categories)
	}
	return result, nil
}

func (s *memoryStore) categoriesPath() string {
	return resolveCategoriesPath(s.dir, s.categoriesFile)
}

// resolveCategoriesPath applies the memory_categories_file override, with
// ~ expansion and relative paths resolved from the working directory.
func resolveCategoriesPath(dir, override string) string {
	if override = strings.TrimSpace(override); override != "" {
		if strings.HasPrefix(override, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				override = filepath.Join(home, override[2:])
			}
		}
		if absolute, err := filepath.Abs(override); err == nil {
			return absolute
		}
		return override
	}
	return filepath.Join(dir, memoryCategoriesFile)
}

// Categories returns the configured memory categories in file order, or nil
// when no categories file exists and categories are free-form.
func (s *memoryStore) Categories() ([]memoryCategory, error) {
	content, err := os.ReadFile(s.categoriesPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseMemoryCategories(string(content)), nil
}

func parseMemoryCategories(content string) []memoryCategory {
	var result []memoryCategory
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, description, _ := strings.Cut(line, ":")
		name = normalizeCategory(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, memoryCategory{Name: name, Description: strings.TrimSpace(description)})
	}
	return result
}

func normalizeCategory(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(name))), "-")
}

func categoryNames(categories []memoryCategory) []string {
	names := make([]string, 0, len(categories))
	for _, category := range categories {
		names = append(names, category.Name)
	}
	return names
}

func hasCategory(categories []memoryCategory, name string) bool {
	for _, category := range categories {
		if category.Name == name {
			return true
		}
	}
	return false
}

// effectiveCategory maps an entry onto a category. Legacy tagged entries use
// the first tag that is a configured category, else their first tag.
func effectiveCategory(entry memoryEntry, categories []memoryCategory) string {
	if name := normalizeCategory(entry.Category); name != "" {
		return name
	}
	for _, tag := range entry.Tags {
		if name := normalizeCategory(tag); name != "" && hasCategory(categories, name) {
			return name
		}
	}
	for _, tag := range entry.Tags {
		if name := normalizeCategory(tag); name != "" {
			return name
		}
	}
	return defaultMemoryCategory
}

// resolveCategory normalizes a requested category and enforces the configured
// list when one exists. An empty request falls back to the default category.
func (s *memoryStore) resolveCategory(name string) (string, error) {
	name = normalizeCategory(name)
	if name == "" {
		name = defaultMemoryCategory
	}
	categories, err := s.Categories()
	if err != nil {
		return "", err
	}
	if len(categories) > 0 && !hasCategory(categories, name) {
		return "", fmt.Errorf("unknown category %q; use one of: %s", name, strings.Join(categoryNames(categories), ", "))
	}
	return name, nil
}

// orderedCategories lists category names with configured ones first, in file
// order, followed by any others alphabetically.
func orderedCategories(entries []memoryEntry, categories []memoryCategory) []string {
	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.Category] = true
	}
	var result []string
	for _, category := range categories {
		result = append(result, category.Name)
		delete(present, category.Name)
	}
	var extra []string
	for name := range present {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	return append(result, extra...)
}

func (s *memoryStore) Save(content, category string) (memoryEntry, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return memoryEntry{}, errors.New("content is required")
	}
	resolved, err := s.resolveCategory(category)
	if err != nil {
		return memoryEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := memoryEntry{
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
		Content:   content,
		Category:  resolved,
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return memoryEntry{}, err
	}
	file, err := os.OpenFile(s.memoriesPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return memoryEntry{}, err
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		return memoryEntry{}, err
	}
	return entry, nil
}

// Search returns memories whose content contains query, optionally limited to
// one category.
func (s *memoryStore) Search(query, category string) string {
	entries, err := s.Load()
	if err != nil {
		return "Could not load memories: " + err.Error()
	}
	query = strings.ToLower(strings.TrimSpace(query))
	category = normalizeCategory(category)
	var matches []memoryEntry
	for _, entry := range entries {
		if category != "" && entry.Category != category {
			continue
		}
		if strings.Contains(strings.ToLower(entry.Content), query) {
			matches = append(matches, entry)
		}
	}
	empty := "No memories matching '" + query + "'."
	if category != "" {
		empty = "No memories matching '" + query + "' in category '" + category + "'."
	}
	return formatMemories(matches, empty)
}

// List returns every memory in a category, or, for an empty category, a
// summary of all categories with their entry counts.
func (s *memoryStore) List(category string) string {
	entries, err := s.Load()
	if err != nil {
		return "Could not load memories: " + err.Error()
	}
	category = normalizeCategory(category)
	if category == "" {
		categories, err := s.Categories()
		if err != nil {
			return "Could not load memory categories: " + err.Error()
		}
		if len(entries) == 0 && len(categories) == 0 {
			return "No memories saved yet."
		}
		counts := map[string]int{}
		for _, entry := range entries {
			counts[entry.Category]++
		}
		var result strings.Builder
		result.WriteString("Memory categories:")
		for _, name := range orderedCategories(entries, categories) {
			fmt.Fprintf(&result, "\n- %s (%d)", name, counts[name])
			for _, configured := range categories {
				if configured.Name == name && configured.Description != "" {
					result.WriteString(": " + configured.Description)
				}
			}
		}
		return result.String()
	}
	var matches []memoryEntry
	for _, entry := range entries {
		if entry.Category == category {
			matches = append(matches, entry)
		}
	}
	return formatMemories(matches, "No memories in category '"+category+"'.")
}

// Update rewrites the content, and optionally the category, of the single
// memory whose content contains query. Ambiguous queries are rejected with
// the candidates so the caller can narrow them.
func (s *memoryStore) Update(query, content, category string) (memoryEntry, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return memoryEntry{}, errors.New("query is required")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return memoryEntry{}, errors.New("content is required")
	}
	resolved := ""
	if strings.TrimSpace(category) != "" {
		var err error
		if resolved, err = s.resolveCategory(category); err != nil {
			return memoryEntry{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.loadUnlocked()
	if err != nil {
		return memoryEntry{}, err
	}
	var matches []int
	for index, entry := range entries {
		if strings.Contains(strings.ToLower(entry.Content), query) {
			matches = append(matches, index)
		}
	}
	switch len(matches) {
	case 0:
		return memoryEntry{}, fmt.Errorf("no memory matches %q", query)
	case 1:
	default:
		candidates := make([]string, 0, len(matches))
		for _, index := range matches {
			candidates = append(candidates, "["+entries[index].Category+"] "+entries[index].Content)
		}
		return memoryEntry{}, fmt.Errorf("%d memories match %q; use a more specific query. Matches:\n%s", len(matches), query, strings.Join(candidates, "\n"))
	}
	target := &entries[matches[0]]
	target.Content = content
	if resolved != "" {
		target.Category = resolved
	}
	if err := s.rewriteUnlocked(entries); err != nil {
		return memoryEntry{}, err
	}
	return *target, nil
}

// Move re-files every memory whose content contains query into category.
func (s *memoryStore) Move(query, category string) (int, string, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return 0, "", errors.New("query is required")
	}
	resolved, err := s.resolveCategory(category)
	if err != nil {
		return 0, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.loadUnlocked()
	if err != nil {
		return 0, "", err
	}
	moved := 0
	for index := range entries {
		if strings.Contains(strings.ToLower(entries[index].Content), query) && entries[index].Category != resolved {
			entries[index].Category = resolved
			moved++
		}
	}
	if moved == 0 {
		return 0, resolved, nil
	}
	return moved, resolved, s.rewriteUnlocked(entries)
}

// Forget removes every memory whose content contains query, optionally
// limited to one category.
func (s *memoryStore) Forget(query, category string) (int, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return 0, errors.New("query is required")
	}
	category = normalizeCategory(category)
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.loadUnlocked()
	if err != nil {
		return 0, err
	}
	var kept []memoryEntry
	for _, entry := range entries {
		matches := strings.Contains(strings.ToLower(entry.Content), query) && (category == "" || entry.Category == category)
		if !matches {
			kept = append(kept, entry)
		}
	}
	removed := len(entries) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	return removed, s.rewriteUnlocked(kept)
}

func (s *memoryStore) rewriteUnlocked(entries []memoryEntry) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(s.memoriesPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *memoryStore) InitialSystemMessages(systemFile string) ([]apiMessage, []recentMessage, int, error) {
	var messages []apiMessage
	if systemFile != "" {
		content, err := os.ReadFile(systemFile)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("read system file: %w", err)
		}
		if text := strings.TrimSpace(string(content)); text != "" {
			messages = append(messages, apiMessage{Role: "system", Content: text})
		}
	}

	if compacted, err := s.CompactedConversations(); err != nil {
		return nil, nil, 0, err
	} else if len(compacted) > 0 {
		messages = append(messages, apiMessage{Role: "system", Content: compactedConversationsSystemBlock(compacted)})
	}

	recent, err := s.RecentMessages()
	if err != nil {
		return nil, nil, 0, err
	}
	if len(recent) > 0 {
		messages = append(messages, apiMessage{Role: "system", Content: recentMessagesSystemBlock(recent)})
	}

	memories, err := s.Load()
	if err != nil {
		return nil, nil, 0, err
	}
	if len(memories) > 0 {
		configured, err := s.Categories()
		if err != nil {
			return nil, nil, 0, err
		}
		messages = append(messages, apiMessage{Role: "system", Content: memorySystemBlock(memories, configured)})
	}
	toolInstructions := memoryToolInstructions
	categories, err := s.Categories()
	if err != nil {
		return nil, nil, 0, err
	}
	if len(categories) > 0 {
		toolInstructions += "\n\n" + memoryCategoriesBlock(categories)
	}
	if newWhoopClient(whoopConfigFromEnvironment(s.dir)).Configured(time.Now()) {
		toolInstructions += "\n\n" + whoopToolInstructions
	}
	messages = append(messages, apiMessage{Role: "system", Content: toolInstructions})
	messages = append(messages, apiMessage{Role: "system", Content: harnessProtocolInstructions})
	return messages, recent, len(memories), nil
}

func (s *memoryStore) AppendRecentMessage(role, content string) error {
	role = strings.ToLower(strings.TrimSpace(role))
	content = strings.TrimSpace(content)
	if role != "user" && role != "assistant" {
		return fmt.Errorf("recent message role must be user or assistant")
	}
	if content == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	messages, err := s.loadRecentMessagesUnlocked()
	if err != nil {
		return err
	}
	messages = append(messages, recentMessage{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Role:      role,
		Content:   content,
	})
	if len(messages) > recentMessageLimit {
		messages = messages[len(messages)-recentMessageLimit:]
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(s.recentMessagesPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, message := range messages {
		if err := encoder.Encode(message); err != nil {
			return err
		}
	}
	return nil
}

func (s *memoryStore) RecentMessages() ([]recentMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadRecentMessagesUnlocked()
}

func (s *memoryStore) loadRecentMessagesUnlocked() ([]recentMessage, error) {
	file, err := os.Open(s.recentMessagesPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var messages []recentMessage
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		var message recentMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		if (message.Role == "user" || message.Role == "assistant") && strings.TrimSpace(message.Content) != "" {
			messages = append(messages, message)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(messages) > recentMessageLimit {
		messages = messages[len(messages)-recentMessageLimit:]
	}
	return messages, nil
}

func (s *memoryStore) compactedConversationPath() string {
	return filepath.Join(s.dir, compactedConversationFile)
}

// SaveCompactedConversation replaces the history with a single entry. It is
// kept for callers that want exactly one summary; the exit flow uses
// AppendCompactedConversation.
func (s *memoryStore) SaveCompactedConversation(summary string, at time.Time) error {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return fmt.Errorf("compacted conversation summary is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCompactedUnlocked([]compactedConversation{{At: at, Summary: summary}})
}

// AppendCompactedConversation adds a datestamped entry and trims the history
// to keep entries, returning the entries that were dropped.
func (s *memoryStore) AppendCompactedConversation(summary string, at time.Time, keep int) ([]compactedConversation, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil, fmt.Errorf("compacted conversation summary is empty")
	}
	if keep < 1 {
		keep = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.loadCompactedUnlocked()
	if err != nil {
		return nil, err
	}
	entries = append(entries, compactedConversation{At: at, Summary: summary})
	var dropped []compactedConversation
	if len(entries) > keep {
		dropped = entries[:len(entries)-keep]
		entries = entries[len(entries)-keep:]
	}
	return dropped, s.writeCompactedUnlocked(entries)
}

// CompactionOverflow returns the entries that AppendCompactedConversation
// would drop if one more entry were added with the given retention.
func (s *memoryStore) CompactionOverflow(keep int) ([]compactedConversation, error) {
	if keep < 1 {
		keep = 1
	}
	entries, err := s.CompactedConversations()
	if err != nil {
		return nil, err
	}
	if len(entries)+1 <= keep {
		return nil, nil
	}
	return entries[:len(entries)+1-keep], nil
}

// CompactedConversations returns the stored session summaries, oldest first.
func (s *memoryStore) CompactedConversations() ([]compactedConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadCompactedUnlocked()
}

// CompactedConversation returns the most recent stored summary, if any.
func (s *memoryStore) CompactedConversation() (compactedConversation, bool, error) {
	entries, err := s.CompactedConversations()
	if err != nil || len(entries) == 0 {
		return compactedConversation{}, false, err
	}
	return entries[len(entries)-1], true, nil
}

func (s *memoryStore) loadCompactedUnlocked() ([]compactedConversation, error) {
	content, err := os.ReadFile(s.compactedConversationPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseCompactedConversations(string(content)), nil
}

func (s *memoryStore) writeCompactedUnlocked(entries []compactedConversation) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	var content strings.Builder
	for _, entry := range entries {
		content.WriteString(compactedSessionHeader + entry.At.Format(time.RFC3339) + "\n\n")
		content.WriteString(strings.TrimSpace(entry.Summary) + "\n\n")
	}
	return os.WriteFile(s.compactedConversationPath(), []byte(content.String()), 0o600)
}

// parseCompactedConversations reads the history format and the earlier
// single-entry format.
func parseCompactedConversations(content string) []compactedConversation {
	text := strings.TrimSpace(content)
	if text == "" {
		return nil
	}
	if !strings.Contains(text, compactedSessionHeader) {
		var entry compactedConversation
		if strings.HasPrefix(text, compactedConversationHeader) {
			header, rest, _ := strings.Cut(text, "\n")
			if at, err := time.Parse(time.RFC3339, strings.TrimSpace(strings.TrimPrefix(header, compactedConversationHeader))); err == nil {
				entry.At = at
				text = rest
			}
		}
		entry.Summary = strings.TrimSpace(text)
		if entry.Summary == "" {
			return nil
		}
		return []compactedConversation{entry}
	}
	var entries []compactedConversation
	var current *compactedConversation
	var body strings.Builder
	flush := func() {
		if current == nil {
			return
		}
		current.Summary = strings.TrimSpace(body.String())
		if current.Summary != "" {
			entries = append(entries, *current)
		}
		body.Reset()
		current = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, compactedSessionHeader) {
			flush()
			current = &compactedConversation{}
			if at, err := time.Parse(time.RFC3339, strings.TrimSpace(strings.TrimPrefix(line, compactedSessionHeader))); err == nil {
				current.At = at
			}
			continue
		}
		if current != nil {
			body.WriteString(line + "\n")
		}
	}
	flush()
	return entries
}

func (s *memoryStore) LastUserActivityTime() time.Time {
	if value := readText(s.lastActivity); value != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func (s *memoryStore) RecordUserActivity(at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.lastActivity, []byte(at.Format(time.RFC3339Nano)), 0o600)
}

func readText(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func formatMemories(entries []memoryEntry, empty string) string {
	if len(entries) == 0 {
		return empty
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, "["+entry.Category+"] "+entry.Content)
	}
	return strings.Join(lines, "\n")
}

// memorySystemBlock renders saved memories grouped by category for startup
// context. Categories are passed in so the configured order is used.
func memorySystemBlock(entries []memoryEntry, categories []memoryCategory) string {
	var result strings.Builder
	result.WriteString("The following are things you have remembered from past conversations, grouped by memory category:")
	for _, name := range orderedCategories(entries, categories) {
		var group []memoryEntry
		for _, entry := range entries {
			if entry.Category == name {
				group = append(group, entry)
			}
		}
		if len(group) == 0 {
			continue
		}
		result.WriteString("\n\n## " + name)
		for _, entry := range group {
			result.WriteString("\n- " + entry.Content)
		}
	}
	return result.String()
}

// leanToolsInstructions is added to startup context only with --lean-tools,
// when tool groups are hidden until the model requests them.
func leanToolsInstructions(whoopConnected bool) string {
	var result strings.Builder
	result.WriteString("Tool exposure: regular turns start with a small core tool set (save_memory, search_memories, get_current_time, request_toolcall, request_tool, and heartbeat scheduling). Other tool groups exist but stay hidden until you ask for them with request_toolcall, naming the capability and the reason; they then stay available for the rest of the session. Request a capability only when the user's current request actually needs it.\n\nHidden tool groups you can expose with request_toolcall:")
	for _, capability := range toolCapabilities {
		if capability.Name == "whoop" && !whoopConnected {
			continue
		}
		result.WriteString("\n- " + capability.Name + ": " + capability.Description)
	}
	return result.String()
}

func memoryCategoriesBlock(categories []memoryCategory) string {
	var result strings.Builder
	result.WriteString("Memory categories are configured. File every memory under exactly one of these categories, choosing the best fit; if something fits none of them, tell the user rather than inventing a category:")
	for _, category := range categories {
		result.WriteString("\n- " + category.Name)
		if category.Description != "" {
			result.WriteString(": " + category.Description)
		}
	}
	return result.String()
}

func compactedConversationsSystemBlock(entries []compactedConversation) string {
	var result strings.Builder
	if len(entries) == 1 {
		result.WriteString("The following is the compacted summary you wrote at the end of the previous session. It is background context from that earlier session, not part of the current conversation.")
	} else {
		fmt.Fprintf(&result, "The following are the compacted summaries you wrote at the end of your last %d sessions, oldest first. They are background context from those earlier sessions, not part of the current conversation.", len(entries))
	}
	for _, entry := range entries {
		result.WriteString("\n\n### Session")
		if !entry.At.IsZero() {
			result.WriteString(" compacted " + humanTimestamp(entry.At, time.Local))
		}
		result.WriteString("\n" + entry.Summary)
	}
	return result.String()
}

func recentMessagesSystemBlock(messages []recentMessage) string {
	var result strings.Builder
	result.WriteString("The following are the most recent user and assistant messages retained from previous conversations, ordered oldest to newest. They are context only and may begin mid-conversation:")
	for _, message := range messages {
		result.WriteString("\n\n")
		result.WriteString(strings.ToUpper(message.Role))
		result.WriteString(":\n")
		result.WriteString(message.Content)
	}
	return result.String()
}

const memoryToolInstructions = `You have access to memory tools: save_memory, list_memories, search_memories, update_memory, move_memory, and forget_memory. Every memory belongs to exactly one category. Use save_memory proactively when the user shares preferences, facts, decisions, or important context worth carrying across sessions, filing each memory under the category that fits it best. Use list_memories to review a whole category before answering questions about that area, search_memories for keyword lookups (optionally within one category), update_memory to correct or refine the wording of one existing memory, move_memory to re-file memories that sit in the wrong category, and forget_memory only when explicitly asked. Prefer answering from the memories already in your context over calling these tools.

If the user's current request is materially blocked by a capability that no tool provides at all, you may call request_tool with a proposed tool name, description, reason, and optional inputs. Use it only for a genuinely missing capability, not for a hidden group (use request_toolcall) and not as a substitute for reasoning. A request_tool call only surfaces a proposal to the user; it does not install, enable, authorize, or execute anything. After its result, briefly explain the request and offer a safe fallback when possible.

The scheduling capability provides schedule_item, list_scheduled_items, and complete_scheduled_item. Use them for tasks, reminders, plans, or follow-ups that must survive across sessions. Always supply the complete intended year, date, and time; call get_current_time first if any part is uncertain. Past times are rejected unless allow_past is true, and you must use allow_past only when the user explicitly intends an overdue or backdated item. For a repeating item, pass both recurrence_unit (hour, day, week, or month) and a positive recurrence_period; omit both fields for a one-off item. The harness expands a recurring series into individual scheduled occurrences. Scheduled items are automatically surfaced in private harness metadata when they become upcoming or due. Mark an occurrence complete only after the user confirms completion and always pass the user's required completion comment to complete_scheduled_item; never invent a completion comment. Completing one occurrence does not stop its recurring series, and completion history is retained temporarily for continuity.

When private schedule metadata contains completion_requests, treat every message as a separate named-user request, even if the same schedule ID was requested before. Use complete_scheduled_item for each request and copy its supplied comment into the tool call. No separate visible acknowledgement is needed, though you should still answer any ordinary user message that accompanied the metadata. When recently_completed contains completed_by and completion_comment, understand that the named person already marked the item complete with that comment; do not try to complete it again.

Heartbeat scheduling protocol:
- ONLY during a regular user turn (not heartbeat), you may use the schedule_next_heartbeat toolcall. Do NOT put the toolcall in the response body.
- Never type the tool name, a pseudo-call, or its JSON arguments as response prose. If you cannot issue a structured tool call, omit it and continue normally; the harness will use its configured default interval.
- During a heartbeat, finish only through the required complete_heartbeat tool. The heartbeat event, tool protocol, reasoning_content, drafts, ordinary prose, and silent result are private and ephemeral. No response has been delivered until you use action notify with the entire user-facing response in message. Action none means the user sees nothing.
- A heartbeat completed with action none recurs at the default interval even if you use tools. A heartbeat completed with action notify stops further heartbeats until the user responds.

The images capability provides request_image, which asks the user to send a photo. State exactly what image would help and why. The user must explicitly approve capture or choose a file; never imply that you can see through the camera or that an image was supplied until the tool result confirms it.`

const whoopToolInstructions = `You may call get_whoop_recovery_sleep when recent recovery or sleep measurements would materially improve your answer. Treat its output as sensitive health-adjacent personal data: retrieve only the number of days needed, do not save it to memory unless the user explicitly asks, and never invent measurements.`
