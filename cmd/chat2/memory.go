package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type memoryStore struct {
	dir                string
	memoriesPath       string
	recentMessagesPath string
	lastActivity       string
	mu                 sync.Mutex
}

const recentMessageLimit = 5

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
	return result, scanner.Err()
}

func (s *memoryStore) Save(content string, tags []string) (memoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := memoryEntry{
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
		Content:   content,
		Tags:      tags,
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

func (s *memoryStore) Search(query string) string {
	entries, err := s.Load()
	if err != nil {
		return "Could not load memories: " + err.Error()
	}
	query = strings.ToLower(query)
	var matches []memoryEntry
	for _, entry := range entries {
		if strings.Contains(strings.ToLower(entry.Content), query) || containsFold(entry.Tags, query) {
			matches = append(matches, entry)
		}
	}
	return formatMemories(matches, "No memories matching '"+query+"'.")
}

func (s *memoryStore) SearchTag(tag string) string {
	entries, err := s.Load()
	if err != nil {
		return "Could not load memories: " + err.Error()
	}
	var matches []memoryEntry
	for _, entry := range entries {
		for _, value := range entry.Tags {
			if strings.EqualFold(value, tag) {
				matches = append(matches, entry)
				break
			}
		}
	}
	return formatMemories(matches, "No memories tagged '"+tag+"'.")
}

func (s *memoryStore) Forget(query string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.loadUnlocked()
	if err != nil {
		return 0, err
	}
	var kept []memoryEntry
	for _, entry := range entries {
		if !strings.Contains(strings.ToLower(entry.Content), strings.ToLower(query)) {
			kept = append(kept, entry)
		}
	}
	removed := len(entries) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return 0, err
	}
	file, err := os.OpenFile(s.memoriesPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, entry := range kept {
		if err := encoder.Encode(entry); err != nil {
			return 0, err
		}
	}
	return removed, nil
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
		messages = append(messages, apiMessage{Role: "system", Content: memorySystemBlock(memories)})
	}
	toolInstructions := memoryToolInstructions
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

func containsFold(values []string, query string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func formatMemories(entries []memoryEntry, empty string) string {
	if len(entries) == 0 {
		return empty
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		line := fmt.Sprintf("[%s] %s", entry.Timestamp, entry.Content)
		if len(entry.Tags) > 0 {
			line += " (tags: " + strings.Join(entry.Tags, ", ") + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func memorySystemBlock(entries []memoryEntry) string {
	var result strings.Builder
	result.WriteString("The following are things you have remembered from past conversations:")
	for _, entry := range entries {
		result.WriteString("\n- [")
		result.WriteString(entry.Timestamp)
		result.WriteString("] ")
		result.WriteString(entry.Content)
		if len(entry.Tags) > 0 {
			result.WriteString(" (tags: ")
			result.WriteString(strings.Join(entry.Tags, ", "))
			result.WriteString(")")
		}
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

const memoryToolInstructions = `You have access to memory tools: save_memory, search_memories, search_memories_by_tag, and forget_memory. Use save_memory proactively when the user shares preferences, facts, decisions, or important context worth carrying across sessions. Search memories when prior context may help, and forget memories when explicitly asked.

You also have native scheduling tools: schedule_item, list_scheduled_items, and complete_scheduled_item. Use them for tasks, reminders, plans, or follow-ups that must survive across sessions. For a repeating item, pass both recurrence_unit (hour, day, week, or month) and a positive recurrence_period; omit both fields for a one-off item. The harness expands a recurring series into individual scheduled occurrences. Scheduled items are automatically surfaced in private harness metadata when they become upcoming or due. Mark an occurrence complete only after the user confirms completion; completing one occurrence does not stop its recurring series, and completion history is retained temporarily for continuity.

Heartbeat scheduling protocol:
- ONLY during a regular user turn (not heartbeat), you may use the schedule_next_heartbeat toolcall. Do NOT put the toolcall in the response body.
- Never type the tool name, a pseudo-call, or its JSON arguments as response prose. If you cannot issue a structured tool call, omit it and continue normally; the harness will use its configured default interval.
- During a heartbeat, finish only through the required complete_heartbeat tool. The heartbeat event, tool protocol, and silent result are private and ephemeral; use action notify only for a complete message worth surfacing to the user.
- A heartbeat without a visible assistant response recurs at the default interval even if you use tools. A visible heartbeat response stops further heartbeats until the user responds.

You may call request_image to request the user send a photo. State exactly what image would help and why. The user must explicitly approve capture or choose a file; never imply that you can see through the camera or that an image was supplied until the tool result confirms it.`

const whoopToolInstructions = `You may call get_whoop_recovery_sleep when recent recovery or sleep measurements would materially improve your answer. Treat its output as sensitive health-adjacent personal data: retrieve only the number of days needed, do not save it to memory unless the user explicitly asks, and never invent measurements.`
