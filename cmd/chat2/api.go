package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type apiClient struct {
	http                       *http.Client
	choice                     modelChoice
	context                    int
	memory                     *memoryStore
	schedule                   *scheduleStore
	whoop                      *whoopClient
	nextHeartbeat              *time.Time
	heartbeatSchedulingAllowed bool
	exposeAllTools             bool
	enabledCapabilities        map[string]bool
	mu                         sync.Mutex
}

// toolCapability is a group of tools that stays hidden from regular turns
// until the model asks for it with request_toolcall (or the user enables it).
// Heartbeats always see every available tool.
type toolCapability struct {
	Name        string
	Description string
	Tools       []string
}

var toolCapabilities = []toolCapability{
	{
		Name:        "scheduling",
		Description: "persistent one-off and recurring reminders, tasks, and events (schedule_item, list_scheduled_items, complete_scheduled_item)",
		Tools:       []string{"schedule_item", "list_scheduled_items", "complete_scheduled_item"},
	},
	{
		Name:        "memory-admin",
		Description: "reviewing and reorganising saved memories by category (list_memories, update_memory, move_memory, forget_memory)",
		Tools:       []string{"list_memories", "update_memory", "move_memory", "forget_memory"},
	},
	{
		Name:        "images",
		Description: "asking the user for a photo or image through a consent prompt (request_image)",
		Tools:       []string{"request_image"},
	},
	{
		Name:        "whoop",
		Description: "the user's recent Whoop recovery and sleep data (get_whoop_recovery_sleep); only works once the user has connected Whoop",
		Tools:       []string{"get_whoop_recovery_sleep"},
	},
}

func findCapability(name string) (toolCapability, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, capability := range toolCapabilities {
		if capability.Name == name {
			return capability, true
		}
	}
	return toolCapability{}, false
}

func capabilityNames() []string {
	names := make([]string, 0, len(toolCapabilities))
	for _, capability := range toolCapabilities {
		names = append(names, capability.Name)
	}
	return names
}

// capabilityFor returns the capability gating a tool, if any. Tools outside
// every capability are core and always exposed.
func capabilityFor(toolName string) (toolCapability, bool) {
	for _, capability := range toolCapabilities {
		for _, name := range capability.Tools {
			if name == toolName {
				return capability, true
			}
		}
	}
	return toolCapability{}, false
}

func (c *apiClient) capabilityEnabled(name string) bool {
	if c.exposeAllTools {
		return true
	}
	return c.enabledCapabilities[name]
}

// EnableCapability exposes a capability for the rest of the session. The
// second result reports whether it was already enabled.
func (c *apiClient) EnableCapability(name string) (toolCapability, bool, error) {
	capability, ok := findCapability(name)
	if !ok {
		return toolCapability{}, false, fmt.Errorf("unknown capability %q; available: %s", strings.TrimSpace(name), strings.Join(capabilityNames(), ", "))
	}
	if c.capabilityEnabled(capability.Name) {
		return capability, true, nil
	}
	if c.enabledCapabilities == nil {
		c.enabledCapabilities = map[string]bool{}
	}
	c.enabledCapabilities[capability.Name] = true
	return capability, false, nil
}

func (c *apiClient) DisableCapability(name string) (toolCapability, error) {
	capability, ok := findCapability(name)
	if !ok {
		return toolCapability{}, fmt.Errorf("unknown capability %q; available: %s", strings.TrimSpace(name), strings.Join(capabilityNames(), ", "))
	}
	delete(c.enabledCapabilities, capability.Name)
	return capability, nil
}

func (c *apiClient) EnabledCapabilities() []string {
	var names []string
	for _, capability := range toolCapabilities {
		if c.capabilityEnabled(capability.Name) {
			names = append(names, capability.Name)
		}
	}
	return names
}

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []apiMessage   `json:"messages"`
	Tools           []toolSpec     `json:"tools"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	Options         map[string]any `json:"options,omitempty"`
	KeepAlive       any            `json:"keep_alive,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   map[string]any `json:"stream_options,omitempty"`
	ReasoningFormat string         `json:"reasoning_format,omitempty"`
	// ChatTemplateKwargs are llama.cpp per-request template variables, used
	// to disable thinking for private harness exchanges.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type toolSpec struct {
	Type     string       `json:"type"`
	Function functionSpec `json:"function"`
}

type functionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResponse struct {
	Choices []struct {
		Message apiMessage `json:"message"`
	} `json:"choices"`
	Usage usage `json:"usage"`
}

type completionResult struct {
	Reply           string
	Reasoning       string
	Usage           usage
	Messages        []apiMessage
	Notices         []string
	ImageRequest    *imageRequest
	NextHeartbeat   *time.Time
	HadToolCalls    bool
	HadVisibleReply bool
}

func newAPIClient(choice modelChoice, contextSize int, memory *memoryStore, timeout time.Duration) *apiClient {
	return &apiClient{
		http:     &http.Client{Timeout: timeout},
		choice:   choice,
		context:  contextSize,
		memory:   memory,
		schedule: newScheduleStore(memory.dir),
		whoop:    newWhoopClient(whoopConfigFromEnvironment(memory.dir)),
	}
}

func discoverModels(ctx context.Context, cfg appConfig) []modelChoice {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		choices []modelChoice
	)

	add := func(found []modelChoice) {
		mu.Lock()
		choices = append(choices, found...)
		mu.Unlock()
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		add(discoverOllama(ctx, cfg.OllamaBaseURL))
	}()
	go func() {
		defer wg.Done()
		add(discoverOpenAI(ctx, cfg.LlamaBaseURL, backendLlama))
	}()
	wg.Wait()

	hasBackend := func(backend backendKind) bool {
		for _, choice := range choices {
			if choice.Backend == backend {
				return true
			}
		}
		return false
	}
	if !hasBackend(backendLlama) {
		choices = append(choices, modelChoice{
			Backend: backendLlama,
			Model:   "gemma4-heretical",
			BaseURL: strings.TrimSuffix(cfg.LlamaBaseURL, "/"),
			APIKey:  "none",
			Detail:  "start ./serve-llama to connect",
		})
	}
	if !hasBackend(backendOllama) {
		choices = append(choices, modelChoice{
			Backend: backendOllama,
			Model:   "gemma4-heretical",
			BaseURL: strings.TrimSuffix(cfg.OllamaBaseURL, "/"),
			APIKey:  "ollama",
			Detail:  "start Ollama to connect",
		})
	}

	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].Online != choices[j].Online {
			return choices[i].Online
		}
		if choices[i].Backend != choices[j].Backend {
			return choices[i].Backend < choices[j].Backend
		}
		return choices[i].Model < choices[j].Model
	})

	return choices
}

func discoverOllama(ctx context.Context, baseURL string) []modelChoice {
	apiRoot := strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1")
	var payload struct {
		Models []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			ModifiedAt string `json:"modified_at"`
			Details    struct {
				ParameterSize     string `json:"parameter_size"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := getJSON(ctx, apiRoot+"/api/tags", &payload); err != nil {
		return nil
	}

	result := make([]modelChoice, 0, len(payload.Models))
	for _, model := range payload.Models {
		detail := "local Ollama model"
		var pieces []string
		if model.Details.ParameterSize != "" {
			pieces = append(pieces, model.Details.ParameterSize)
		}
		if model.Details.QuantizationLevel != "" {
			pieces = append(pieces, model.Details.QuantizationLevel)
		}
		if len(pieces) > 0 {
			detail = strings.Join(pieces, " · ")
		}
		result = append(result, modelChoice{
			Backend: backendOllama,
			Model:   model.Name,
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			APIKey:  "ollama",
			Online:  true,
			Detail:  detail,
		})
	}
	return result
}

func discoverOpenAI(ctx context.Context, baseURL string, backend backendKind) []modelChoice {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, strings.TrimSuffix(baseURL, "/")+"/models", &payload); err != nil {
		return nil
	}
	result := make([]modelChoice, 0, len(payload.Data))
	for _, model := range payload.Data {
		result = append(result, modelChoice{
			Backend: backend,
			Model:   model.ID,
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			APIKey:  "none",
			Online:  true,
			Detail:  "vision + MTP server",
		})
	}
	return result
}

func getJSON(ctx context.Context, url string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 1800 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target)
}

func (c *apiClient) post(ctx context.Context, endpoint string, payload, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimSuffix(c.choice.BaseURL, "/")+endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.choice.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s is unavailable: %w", c.choice.Backend, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("%s returned %s: %s", c.choice.Backend, resp.Status, strings.TrimSpace(string(message)))
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func imageParts(prompt string, paths []string) ([]contentPart, error) {
	return imagePartsWithVault(prompt, paths, nil)
}

func imagePartsWithVault(prompt string, paths []string, vault *captureVault) ([]contentPart, error) {
	parts := []contentPart{{Type: "text", Text: prompt}}
	for _, path := range paths {
		var (
			data []byte
			err  error
		)
		mediaPath := path
		if vault != nil && vault.Owns(path) {
			data, err = vault.Open(path)
			mediaPath = strings.TrimSuffix(path, ".enc")
		} else {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, fmt.Errorf("read image %q: %w", path, err)
		}
		mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(mediaPath)))
		if mediaType == "" {
			clear(data)
			return nil, fmt.Errorf("unsupported image extension: %s", filepath.Ext(mediaPath))
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		clear(data)
		parts = append(parts, contentPart{
			Type: "image_url",
			ImageURL: &imageURL{
				URL: "data:" + mediaType + ";base64," + encoded,
			},
		})
	}
	return parts, nil
}

var tools = []toolSpec{
	{
		Type: "function",
		Function: functionSpec{
			Name:        "get_whoop_recovery_sleep",
			Description: "Retrieve the user's recent Whoop recovery and sleep records. Use this when current or historical recovery, HRV, resting heart rate, sleep duration, sleep stages, performance, consistency, or efficiency would help. The user must complete the separate local Whoop OAuth setup first.",
			Parameters: objectSchema(
				map[string]any{
					"days": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     30,
						"description": "Number of calendar days of recent Whoop data to retrieve. Defaults to 7.",
					},
				},
				nil,
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "request_toolcall",
			Description: "Expose a hidden group of existing tools for the rest of this session when the user's request needs it. Capabilities: " + capabilityCatalogue() + ". The tools become available on your next tool-calling round. Use request_tool instead for a capability that does not exist at all.",
			Parameters: objectSchema(
				map[string]any{
					"capability": map[string]any{
						"type":        "string",
						"enum":        capabilityNames(),
						"description": "The capability to expose.",
					},
					"reason": stringSchema("Why the current request needs these tools."),
				},
				[]string{"capability", "reason"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "request_tool",
			Description: "Request a tool capability that is genuinely missing and materially blocks the user's current request. This surfaces the request to the user; it does not install, enable, authorize, or execute the proposed tool. Do not request tools already available, and use request_toolcall for hidden tool groups that already exist. After the tool result, briefly explain the request and any safe fallback in your visible response.",
			Parameters: objectSchema(
				map[string]any{
					"name":        stringSchema("A concise proposed snake_case name for the missing tool, such as create_calendar_event."),
					"description": stringSchema("A self-contained description of what the proposed tool should do."),
					"reason":      stringSchema("Why the missing capability is needed for the user's current request."),
					"inputs":      stringSchema("Optional concise description of the inputs the proposed tool would need."),
				},
				[]string{"name", "description", "reason"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "request_image",
			Description: "Ask the user to provide a current image when visual information is genuinely needed. This pauses the conversation and presents a consent-based camera or file-picker prompt. Never claim an image was provided until the tool result confirms it.",
			Parameters: objectSchema(
				map[string]any{
					"prompt": stringSchema("A concise instruction describing what the user should photograph or provide."),
					"reason": stringSchema("Why seeing this image would help answer the current request."),
				},
				[]string{"prompt", "reason"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "schedule_item",
			Description: "Schedule a one-off or recurring task, reminder, or event beginning at a specific local date and time. Supply the complete year, date, and time as RFC3339 or YYYY-MM-DD HH:MM; call get_current_time first when any part is uncertain. Past times are rejected unless allow_past is explicitly true, which is only for an intentionally overdue or backdated item. For recurrence, provide both recurrence_unit and recurrence_period. The harness persists the series and materializes it as individual schedule items through the next 24 hours plus one later occurrence. Omit both recurrence fields for a one-off item.",
			Parameters: objectSchema(
				map[string]any{
					"time":  stringSchema("When the item becomes due, including its year, as RFC3339 or local YYYY-MM-DD HH:MM."),
					"event": stringSchema("A concise, self-contained description of what should happen or be remembered."),
					"allow_past": map[string]any{
						"type":        "boolean",
						"description": "Set true only when intentionally creating an already-overdue or backdated item. Omit or use false for normal future scheduling.",
					},
					"recurrence_unit": map[string]any{
						"type":        "string",
						"enum":        []string{"hour", "day", "week", "month"},
						"description": "Optional recurrence interval unit. Provide together with recurrence_period.",
					},
					"recurrence_period": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     maxRecurrencePeriod,
						"description": "Optional number of recurrence units between occurrences, such as 1 for every hour or 2 for every two weeks. Provide together with recurrence_unit.",
					},
				},
				[]string{"time", "event"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "list_scheduled_items",
			Description: "List all pending scheduled items and recently completed items, including their IDs, status, completion author, and completion comment.",
			Parameters:  objectSchema(nil, nil),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "complete_scheduled_item",
			Description: "Mark a scheduled item complete with the user's completion comment while retaining it in recent schedule history. Copy the user's supplied comment faithfully; do not invent one.",
			Parameters: objectSchema(
				map[string]any{
					"id": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"description": "The scheduled item ID returned by schedule_item or list_scheduled_items.",
					},
					"comment": stringSchema("The user's required comment about completing this item."),
				},
				[]string{"id", "comment"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "get_current_time",
			Description: "Returns the exact current local year, date, time, and timezone. Call this before scheduling whenever any part of the intended timestamp is uncertain.",
			Parameters:  objectSchema(nil, nil),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "schedule_next_heartbeat",
			Description: "Choose when the harness should next give you an opportunity to follow up. Invoke this function only through the API's structured tool-calling channel during a regular user turn. Put the when argument in the tool call; never write the function name, a pseudo-call, or its arguments in assistant response content. If you cannot issue a structured tool call, omit it and let the harness use its default interval. After the tool result, continue with your normal visible response. Do not invoke this function from a heartbeat. The when value must be a positive Go-style duration such as 30m or 2h, or a future RFC3339 timestamp.",
			Parameters: objectSchema(
				map[string]any{"when": stringSchema("A positive duration from now, such as 30m or 2h, or a future RFC3339 timestamp.")},
				[]string{"when"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "save_memory",
			Description: "Save something worth remembering for future conversations, filed under one memory category.",
			Parameters: objectSchema(
				map[string]any{
					"content":  stringSchema("A clear, self-contained statement to remember."),
					"category": stringSchema("The memory category to file this under. When categories are configured, use one of them exactly."),
				},
				[]string{"content", "category"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "list_memories",
			Description: "List every memory in one category, or omit the category to see all categories with their entry counts.",
			Parameters: objectSchema(
				map[string]any{"category": stringSchema("Category to list. Omit for a summary of all categories.")},
				nil,
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "search_memories",
			Description: "Search previously saved memories by keyword, optionally within one category.",
			Parameters: objectSchema(
				map[string]any{
					"query":    stringSchema("Keyword or phrase to search for."),
					"category": stringSchema("Optional category to search within."),
				},
				[]string{"query"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "update_memory",
			Description: "Rewrite the content of one existing memory, found by keyword, optionally moving it to another category. The keyword must match exactly one memory.",
			Parameters: objectSchema(
				map[string]any{
					"query":    stringSchema("Keyword identifying the single memory to update."),
					"content":  stringSchema("The complete new statement that replaces the old content."),
					"category": stringSchema("Optional new category for the memory."),
				},
				[]string{"query", "content"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "move_memory",
			Description: "Re-file memories matching a keyword into a different category.",
			Parameters: objectSchema(
				map[string]any{
					"query":    stringSchema("Keyword identifying the memories to move."),
					"category": stringSchema("The category to move them into."),
				},
				[]string{"query", "category"},
			),
		},
	},
	{
		Type: "function",
		Function: functionSpec{
			Name:        "forget_memory",
			Description: "Remove memories matching a keyword, optionally within one category.",
			Parameters: objectSchema(
				map[string]any{
					"query":    stringSchema("Keyword identifying memories to remove."),
					"category": stringSchema("Optional category to limit the removal to."),
				},
				[]string{"query"},
			),
		},
	},
}

func capabilityCatalogue() string {
	parts := make([]string, 0, len(toolCapabilities))
	for _, capability := range toolCapabilities {
		parts = append(parts, capability.Name+" = "+capability.Description)
	}
	return strings.Join(parts, "; ")
}

func parseImageRequest(call toolCall) (*imageRequest, error) {
	var args struct {
		Prompt string `json:"prompt"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return nil, err
	}
	args.Prompt = strings.TrimSpace(args.Prompt)
	args.Reason = strings.TrimSpace(args.Reason)
	if args.Prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if args.Reason == "" {
		return nil, errors.New("reason is required")
	}
	return &imageRequest{
		ToolCallID: call.ID,
		Prompt:     args.Prompt,
		Reason:     args.Reason,
	}, nil
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	result := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		result["required"] = required
	}
	return result
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func (c *apiClient) runTool(call toolCall) (string, string) {
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "Invalid tool arguments: " + err.Error(), ""
	}
	stringArg := func(name string) string {
		value, _ := args[name].(string)
		return value
	}

	switch call.Function.Name {
	case "get_whoop_recovery_sleep":
		days := 7
		if _, provided := args["days"]; provided {
			var err error
			days, err = positiveIntArg(args, "days")
			if err != nil || days > 30 {
				return "Could not retrieve Whoop data: days must be an integer from 1 to 30", ""
			}
		}
		result, err := c.whoop.RecoverySleep(context.Background(), days, time.Now())
		if err != nil {
			return "Could not retrieve Whoop data: " + err.Error(), ""
		}
		return encodeToolResult(result), fmt.Sprintf("retrieved %d day(s) of Whoop data", days)
	case "request_toolcall":
		reason := strings.TrimSpace(stringArg("reason"))
		if reason == "" {
			return "Could not expose tools: reason is required.", ""
		}
		capability, already, err := c.EnableCapability(stringArg("capability"))
		if err != nil {
			return "Could not expose tools: " + err.Error(), ""
		}
		status := "enabled"
		if already {
			status = "already_enabled"
		}
		result := encodeToolResult(map[string]any{
			"status":     status,
			"capability": capability.Name,
			"tools":      capability.Tools,
			"note":       "These tools are available from your next tool-calling round for the rest of this session.",
		})
		if already {
			return result, ""
		}
		return result, "enabled capability " + capability.Name
	case "request_tool":
		name := strings.TrimSpace(stringArg("name"))
		description := strings.TrimSpace(stringArg("description"))
		reason := strings.TrimSpace(stringArg("reason"))
		inputs := strings.TrimSpace(stringArg("inputs"))
		if name == "" || description == "" || reason == "" {
			return "Could not request tool: name, description, and reason are required.", ""
		}
		request := map[string]any{
			"status":      "requested",
			"name":        name,
			"description": description,
			"reason":      reason,
			"effect":      "The request was surfaced to the user. No tool was installed, enabled, authorized, or executed.",
		}
		if inputs != "" {
			request["inputs"] = inputs
		}
		return encodeToolResult(request), "tool requested: " + name
	case "get_current_time":
		return time.Now().Format("2006-01-02 15:04:05 MST"), ""
	case "schedule_next_heartbeat":
		if !c.heartbeatSchedulingAllowed {
			return "The next heartbeat can only be scheduled while replying to a real user message.", ""
		}
		when, err := parseNextHeartbeat(stringArg("when"), time.Now())
		if err != nil {
			return "Could not schedule the next heartbeat: " + err.Error(), ""
		}
		c.nextHeartbeat = &when
		return encodeToolResult(map[string]any{
			"scheduled_for": when.Format(time.RFC3339),
			"after":         time.Until(when).Round(time.Second).String(),
		}), "next heartbeat " + when.Format("2006-01-02 15:04 MST")
	case "schedule_item":
		now := time.Now()
		when, err := parseScheduleTime(stringArg("time"), now.Location())
		if err != nil {
			return "Could not schedule item: " + err.Error(), ""
		}
		allowPast, _ := args["allow_past"].(bool)
		if when.Before(now.Truncate(time.Minute)) && !allowPast {
			return fmt.Sprintf(
				"Could not schedule item: %s is in the past relative to %s. Only retry with allow_past=true if the user explicitly intends an overdue or backdated item.",
				when.Format(time.RFC3339),
				now.Format(time.RFC3339),
			), ""
		}
		unit := strings.ToLower(strings.TrimSpace(stringArg("recurrence_unit")))
		_, hasPeriod := args["recurrence_period"]
		if (unit == "" && hasPeriod) || (unit != "" && !hasPeriod) {
			return "Could not schedule item: recurrence_unit and recurrence_period must be provided together", ""
		}
		var entry scheduleEntry
		if unit == "" {
			entry, err = c.schedule.Add(when, stringArg("event"), now)
		} else {
			period, periodErr := positiveIntArg(args, "recurrence_period")
			if periodErr != nil {
				return "Could not schedule item: " + periodErr.Error(), ""
			}
			entry, err = c.schedule.AddRecurring(when, stringArg("event"), unit, period, now)
		}
		if err != nil {
			return "Could not schedule item: " + err.Error(), ""
		}
		notice := fmt.Sprintf("scheduled item #%d", entry.ID)
		if entry.SeriesID != 0 {
			notice = fmt.Sprintf("scheduled recurring series #%d", entry.SeriesID)
		}
		return encodeToolResult(scheduleView(entry, now)), notice
	case "list_scheduled_items":
		now := time.Now()
		if err := c.schedule.MaterializeRecurrences(now, defaultScheduleUpcomingWindow); err != nil {
			return "Could not list scheduled items: " + err.Error(), ""
		}
		entries, err := c.schedule.List()
		if err != nil {
			return "Could not list scheduled items: " + err.Error(), ""
		}
		recentStart := now.Add(-defaultScheduleHistoryWindow)
		visible := entries[:0]
		for _, entry := range entries {
			if entry.CompletedAt == nil || !entry.CompletedAt.Before(recentStart) {
				visible = append(visible, entry)
			}
		}
		return encodeToolResult(scheduleViews(visible, now)), ""
	case "complete_scheduled_item":
		id, err := positiveIntArg(args, "id")
		if err != nil {
			return "Could not complete scheduled item: " + err.Error(), ""
		}
		comment := strings.TrimSpace(stringArg("comment"))
		if comment == "" {
			return "Could not complete scheduled item: completion comment is required", ""
		}
		now := time.Now()
		entry, err := c.schedule.Complete(id, comment, "agent", now)
		if err != nil {
			return "Could not complete scheduled item: " + err.Error(), ""
		}
		return encodeToolResult(scheduleView(entry, now)), fmt.Sprintf("completed scheduled item #%d", entry.ID)
	case "save_memory":
		entry, err := c.memory.Save(stringArg("content"), stringArg("category"))
		if err != nil {
			return "Could not save memory: " + err.Error(), ""
		}
		return "Saved in " + entry.Category + ": " + entry.Content, "memory saved"
	case "list_memories":
		return c.memory.List(stringArg("category")), ""
	case "search_memories":
		return c.memory.Search(stringArg("query"), stringArg("category")), ""
	case "update_memory":
		entry, err := c.memory.Update(stringArg("query"), stringArg("content"), stringArg("category"))
		if err != nil {
			return "Could not update memory: " + err.Error(), ""
		}
		return "Updated in " + entry.Category + ": " + entry.Content, "memory updated"
	case "move_memory":
		count, category, err := c.memory.Move(stringArg("query"), stringArg("category"))
		if err != nil {
			return "Could not move memory: " + err.Error(), ""
		}
		if count == 0 {
			return "No memories matching '" + strings.TrimSpace(stringArg("query")) + "' needed moving.", ""
		}
		return fmt.Sprintf("Moved %d memory entry(s) to %s.", count, category), "memory updated"
	case "forget_memory":
		count, err := c.memory.Forget(stringArg("query"), stringArg("category"))
		if err != nil {
			return "Could not forget memory: " + err.Error(), ""
		}
		return fmt.Sprintf("Removed %d memory entry(s).", count), fmt.Sprintf("removed %d memory entry(s)", count)
	default:
		return "Unknown tool: " + call.Function.Name, ""
	}
}

func parseNextHeartbeat(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("when is required")
	}
	if delay, err := time.ParseDuration(value); err == nil {
		if delay <= 0 {
			return time.Time{}, errors.New("duration must be positive")
		}
		return now.Add(delay), nil
	}
	when, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("when must be a duration such as 30m or a future RFC3339 timestamp")
	}
	if !when.After(now) {
		return time.Time{}, errors.New("timestamp must be in the future")
	}
	return when, nil
}

func positiveIntArg(arguments map[string]any, name string) (int, error) {
	value, ok := arguments[name].(float64)
	if !ok || value < 1 || value != float64(int(value)) {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return int(value), nil
}

// decodeToolArguments parses a tool call's JSON arguments strictly.
func decodeToolArguments(arguments string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("arguments must contain one JSON object")
	}
	return nil
}

func encodeToolResult(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"error":"could not encode tool result"}`
	}
	return string(encoded)
}
