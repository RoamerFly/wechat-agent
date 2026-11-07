package adapter

import "encoding/json"

// VoiceConversionSchema describes optional conversion of retained voice media.
// It is intentionally shared with the host so the advertised MCP schema stays
// identical wherever a WeChat media submission is exposed.
const VoiceConversionSchema = `{"type":"object","description":"Voice conversion from retained original. start_seconds default0; duration_seconds default0 means remainder; segment_seconds default60, actual WeChat maximum60. timeout_seconds default90, adjustable execution budget. File preserves original and does not accept conversion.","properties":{"start_seconds":{"type":"number","minimum":0},"duration_seconds":{"type":"number","minimum":0},"segment_seconds":{"type":"number","exclusiveMinimum":0,"maximum":60},"timeout_seconds":{"type":"number","exclusiveMinimum":0}},"additionalProperties":false}`

// WithVoiceConversion adds the unchanged voice conversion schema to an object
// schema's properties. The host supplies only constant, validated schemas.
func WithVoiceConversion(schema string) string {
	var v map[string]any
	_ = json.Unmarshal([]byte(schema), &v)
	var conversion any
	_ = json.Unmarshal([]byte(VoiceConversionSchema), &conversion)
	v["properties"].(map[string]any)["conversion"] = conversion
	b, _ := json.Marshal(v)
	return string(b)
}
