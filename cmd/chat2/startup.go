package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type sessionLabels struct {
	User      string
	Assistant string
}

type startupResult struct {
	Messages []apiMessage
	Labels   sessionLabels
	Usage    usage
}

type harnessExchange struct {
	Name        string
	Instruction string
	Tool        toolSpec
	Validate    func(toolCall) error
}

type harnessExchangeResult struct {
	Messages []apiMessage
	Call     toolCall
	Usage    usage
}

const harnessProtocolInstructions = `[HARNESS PROTOCOL]
The local chat harness may send private control-plane requests before or between user-visible turns. Harness messages are system instructions, not user messages. When a harness request provides a response tool, complete the request by calling that tool with arguments matching its schema; do not answer the request in conversational prose. The harness validates the call and returns a tool acknowledgement. Keep the resulting configuration in mind during later conversation. Do not expose private harness exchanges to the user unless the user explicitly asks about them.`

const sessionLabelRequest = `[HARNESS REQUEST: session_labels]
Startup context and memories are now loaded. Before the user-facing conversation begins, choose two short, distinctive terminal labels informed by that context when appropriate:
- assistant_label: a label for yourself
- user_label: a respectful label for the user

Use 1–24 visible characters for each, make them different from one another, and call configure_session_labels exactly once. Do not produce a conversational response.`

var sessionLabelsTool = toolSpec{
	Type: "function",
	Function: functionSpec{
		Name:        "configure_session_labels",
		Description: "Complete the private harness startup protocol by selecting terminal transcript labels for the assistant and user.",
		Parameters: objectSchema(
			map[string]any{
				"assistant_label": stringSchema("A short terminal label the assistant chooses for itself."),
				"user_label":      stringSchema("A short, respectful terminal label for the user."),
			},
			[]string{"assistant_label", "user_label"},
		),
	},
}

func (c *apiClient) startup(ctx context.Context, messages []apiMessage) (startupResult, error) {
	var labels sessionLabels
	exchange, err := c.runHarnessExchange(ctx, messages, harnessExchange{
		Name:        "session_labels",
		Instruction: sessionLabelRequest,
		Tool:        sessionLabelsTool,
		Validate: func(call toolCall) error {
			parsed, err := parseSessionLabels(call)
			if err == nil {
				labels = parsed
			}
			return err
		},
	})
	if err != nil {
		return startupResult{}, err
	}
	exchange.Messages = append(exchange.Messages, apiMessage{
		Role: "system",
		Content: fmt.Sprintf(
			"[HARNESS CONFIGURATION]\nUse %q as your terminal label and %q as the user's terminal label for this session.",
			labels.Assistant,
			labels.User,
		),
	})
	return startupResult{
		Messages: exchange.Messages,
		Labels:   labels,
		Usage:    exchange.Usage,
	}, nil
}

func (c *apiClient) runHarnessExchange(
	ctx context.Context,
	messages []apiMessage,
	exchange harnessExchange,
) (harnessExchangeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	working := append([]apiMessage(nil), messages...)
	working = append(working, apiMessage{Role: "system", Content: exchange.Instruction})
	var lastUsage usage
	for attempt := 1; attempt <= 3; attempt++ {
		request := chatRequest{
			Model:    c.choice.Model,
			Messages: working,
			Tools:    []toolSpec{exchange.Tool},
		}
		if c.choice.Backend == backendOllama {
			request.Options = map[string]any{"num_ctx": c.context}
			request.KeepAlive = -1
		}

		var response chatResponse
		if err := c.post(ctx, "/chat/completions", request, &response); err != nil {
			return harnessExchangeResult{}, err
		}
		if len(response.Choices) == 0 {
			return harnessExchangeResult{}, errors.New("server returned no completion choices during harness exchange")
		}
		lastUsage = response.Usage
		message := response.Choices[0].Message
		working = append(working, message)

		var validationError error
		for _, call := range message.ToolCalls {
			if call.Function.Name != exchange.Tool.Function.Name {
				validationError = fmt.Errorf("unexpected tool %q", call.Function.Name)
				if call.ID != "" {
					working = append(working, apiMessage{
						Role:       "tool",
						ToolCallID: call.ID,
						Content:    "This tool is not available during the current harness request.",
					})
				}
				continue
			}
			if call.ID == "" {
				return harnessExchangeResult{}, errors.New("harness tool call ID is missing")
			}
			if err := exchange.Validate(call); err != nil {
				validationError = err
				working = append(working, apiMessage{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    "Harness validation failed: " + err.Error(),
				})
				continue
			}

			acknowledgement, _ := json.Marshal(map[string]any{
				"accepted": true,
				"request":  exchange.Name,
			})
			working = append(working,
				apiMessage{Role: "tool", ToolCallID: call.ID, Content: string(acknowledgement)},
				apiMessage{
					Role: "system",
					Content: fmt.Sprintf(
						"[HARNESS COMPLETE: %s]\nThe configuration was accepted. The user-facing conversation may now begin.",
						exchange.Name,
					),
				},
			)
			return harnessExchangeResult{Messages: working, Call: call, Usage: lastUsage}, nil
		}

		reason := "the required tool was not called"
		if validationError != nil {
			reason = validationError.Error()
		}
		working = append(working, apiMessage{
			Role: "system",
			Content: fmt.Sprintf(
				"[HARNESS RETRY: %s]\nAttempt %d was not accepted because %s. Call %s with valid arguments and do not respond in prose.",
				exchange.Name,
				attempt,
				reason,
				exchange.Tool.Function.Name,
			),
		})
	}
	return harnessExchangeResult{}, fmt.Errorf("harness request %q failed after 3 attempts", exchange.Name)
}

func parseSessionLabels(call toolCall) (sessionLabels, error) {
	var arguments struct {
		Assistant string `json:"assistant_label"`
		User      string `json:"user_label"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
		return sessionLabels{}, fmt.Errorf("invalid label arguments: %w", err)
	}
	assistant, err := validateTerminalLabel("assistant_label", arguments.Assistant)
	if err != nil {
		return sessionLabels{}, err
	}
	user, err := validateTerminalLabel("user_label", arguments.User)
	if err != nil {
		return sessionLabels{}, err
	}
	if strings.EqualFold(assistant, user) {
		return sessionLabels{}, errors.New("assistant_label and user_label must be different")
	}
	return sessionLabels{Assistant: assistant, User: user}, nil
}

func validateTerminalLabel(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	count := utf8.RuneCountInString(value)
	if count < 1 || count > 24 {
		return "", fmt.Errorf("%s must contain 1–24 characters", field)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%s contains a control character", field)
		}
	}
	return value, nil
}
