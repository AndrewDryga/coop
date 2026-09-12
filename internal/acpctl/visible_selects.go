package acpctl

import "encoding/json"

// Hide controls only at the editor boundary. Native/cache options must retain a
// sole model or effort value so target validation and replay still see its truth.
func visibleConfigOptions(raw json.RawMessage) json.RawMessage {
	var options []json.RawMessage
	if json.Unmarshal(raw, &options) != nil {
		return raw
	}
	visible := make([]json.RawMessage, 0, len(options))
	for _, option := range options {
		var head struct {
			Type    string            `json:"type"`
			Current *string           `json:"currentValue"`
			Options []json.RawMessage `json:"options"`
		}
		if json.Unmarshal(option, &head) == nil && head.Type == "select" && head.Current != nil {
			if value, sole := singleOptionValue(head.Options); sole && value == *head.Current {
				continue
			}
		}
		visible = append(visible, option)
	}
	encoded, err := json.Marshal(visible)
	if err != nil {
		return raw
	}
	return encoded
}

func singleOptionValue(options []json.RawMessage) (string, bool) {
	var value *string
	// Count leaves, not groups: valid empty groups offer no additional choice.
	for len(options) > 0 {
		raw := options[0]
		options = options[1:]
		var option struct {
			Value   *string            `json:"value"`
			Options *[]json.RawMessage `json:"options"`
		}
		if json.Unmarshal(raw, &option) != nil {
			return "", false
		}
		switch {
		case option.Value != nil && option.Options == nil:
			if value != nil {
				return "", false
			}
			value = option.Value
		case option.Value == nil && option.Options != nil:
			options = append(options, *option.Options...)
		default:
			return "", false
		}
	}
	if value == nil {
		return "", false
	}
	return *value, true
}
