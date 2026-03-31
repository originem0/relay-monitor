package responsefmt

import (
	"encoding/json"
	"fmt"
	"strings"
)

type PayloadFacts struct {
	OutputItems     int
	HasFunctionCall bool
}

type StreamEventFacts struct {
	HasFunctionCall bool
}

func ValidatePayload(body []byte, requireToolCall bool) error {
	_, err := InspectPayload(body, requireToolCall)
	return err
}

func InspectPayload(body []byte, requireToolCall bool) (PayloadFacts, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return PayloadFacts{}, fmt.Errorf("invalid JSON: %w", err)
	}

	outputVal, ok := payload["output"]
	if !ok {
		return PayloadFacts{}, fmt.Errorf("missing output field")
	}
	output, ok := outputVal.([]any)
	if !ok {
		return PayloadFacts{}, fmt.Errorf("output is not an array")
	}

	facts := PayloadFacts{OutputItems: len(output)}
	for i, itemVal := range output {
		hasFunctionCall, err := validateOutputItem(itemVal, "output", i)
		if err != nil {
			return PayloadFacts{}, err
		}
		if hasFunctionCall {
			facts.HasFunctionCall = true
		}
	}

	if requireToolCall && !facts.HasFunctionCall {
		return PayloadFacts{}, fmt.Errorf("required tool call missing from output")
	}
	return facts, nil
}

func ValidateStreamEvent(body []byte) (StreamEventFacts, error) {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return StreamEventFacts{}, fmt.Errorf("invalid stream JSON: %w", err)
	}

	eventType, _ := event["type"].(string)
	if eventType == "" {
		return StreamEventFacts{}, fmt.Errorf("stream event missing type")
	}

	facts := StreamEventFacts{}
	for _, key := range []string{"item", "output_item"} {
		if raw, ok := event[key]; ok {
			hasFunctionCall, err := validateOutputItem(raw, key, -1)
			if err != nil {
				return StreamEventFacts{}, err
			}
			if hasFunctionCall {
				facts.HasFunctionCall = true
			}
		}
	}
	for _, key := range []string{"part", "content_part"} {
		if raw, ok := event[key]; ok {
			if err := validatePart(raw, key, key, -1, -1); err != nil {
				return StreamEventFacts{}, err
			}
		}
	}

	switch eventType {
	case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta":
		if _, ok := event["delta"].(string); !ok {
			return StreamEventFacts{}, fmt.Errorf("%s missing delta", eventType)
		}
	case "response.output_text.done", "response.refusal.done":
		if _, ok := event["text"].(string); !ok {
			return StreamEventFacts{}, fmt.Errorf("%s missing text", eventType)
		}
	case "response.function_call_arguments.done":
		if _, ok := event["arguments"].(string); !ok {
			return StreamEventFacts{}, fmt.Errorf("%s missing arguments", eventType)
		}
	}

	if strings.Contains(eventType, "function_call") {
		if name, _ := event["name"].(string); name != "" {
			facts.HasFunctionCall = true
		}
	}

	return facts, nil
}

func validateOutputItem(raw any, field string, index int) (bool, error) {
	item, ok := raw.(map[string]any)
	if !ok {
		if index >= 0 {
			return false, fmt.Errorf("%s[%d] is not an object", field, index)
		}
		return false, fmt.Errorf("%s is not an object", field)
	}

	itemType, _ := item["type"].(string)
	if itemType == "" {
		if index >= 0 {
			return false, fmt.Errorf("%s[%d] missing type", field, index)
		}
		return false, fmt.Errorf("%s missing type", field)
	}

	hasFunctionCall := false
	if itemType == "function_call" {
		hasFunctionCall = true
		if name, _ := item["name"].(string); name == "" {
			if index >= 0 {
				return false, fmt.Errorf("%s[%d] function_call missing name", field, index)
			}
			return false, fmt.Errorf("%s function_call missing name", field)
		}
	}
	if err := validateParts(item["content"], field, index, "content"); err != nil {
		return false, err
	}
	if err := validateParts(item["summary"], field, index, "summary"); err != nil {
		return false, err
	}
	return hasFunctionCall, nil
}

func validateParts(raw any, field string, index int, child string) error {
	if raw == nil {
		return nil
	}
	parts, ok := raw.([]any)
	if !ok {
		if index >= 0 {
			return fmt.Errorf("%s[%d].%s is not an array", field, index, child)
		}
		return fmt.Errorf("%s.%s is not an array", field, child)
	}
	for partIndex, partVal := range parts {
		partField := child
		if index < 0 {
			partField = field
		}
		if err := validatePart(partVal, field, partField, index, partIndex); err != nil {
			return err
		}
	}
	return nil
}

func validatePart(raw any, field string, child string, index int, partIndex int) error {
	part, ok := raw.(map[string]any)
	if !ok {
		switch {
		case index >= 0 && partIndex >= 0:
			return fmt.Errorf("%s[%d].%s[%d] is not an object", field, index, child, partIndex)
		case partIndex >= 0:
			return fmt.Errorf("%s[%d] is not an object", child, partIndex)
		default:
			return fmt.Errorf("%s is not an object", field)
		}
	}

	partType, _ := part["type"].(string)
	if partType == "" {
		switch {
		case index >= 0 && partIndex >= 0:
			return fmt.Errorf("%s[%d].%s[%d] missing type", field, index, child, partIndex)
		case partIndex >= 0:
			return fmt.Errorf("%s[%d] missing type", child, partIndex)
		default:
			return fmt.Errorf("%s missing type", field)
		}
	}

	switch partType {
	case "output_text", "input_text", "text", "summary_text":
		if _, ok := part["text"].(string); !ok {
			switch {
			case index >= 0 && partIndex >= 0:
				return fmt.Errorf("%s[%d].%s[%d] missing text", field, index, child, partIndex)
			case partIndex >= 0:
				return fmt.Errorf("%s[%d] missing text", child, partIndex)
			default:
				return fmt.Errorf("%s missing text", field)
			}
		}
	}
	return nil
}
