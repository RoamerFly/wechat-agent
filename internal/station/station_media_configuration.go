package station

import (
	"encoding/json"
	"slices"
	"strings"

	"wechat-bot/internal/pluginhost"
)

// Shared media configuration validation. Applications pass their own scoped
// store configuration; this helper never selects another account's secrets.
func stationVisionConfigured(c pluginhost.Configuration) bool {
	var endpoint, model string
	return json.Unmarshal(c.Values["vision_endpoint"], &endpoint) == nil && strings.TrimSpace(endpoint) != "" &&
		json.Unmarshal(c.Values["vision_model"], &model) == nil && strings.TrimSpace(model) != "" && slices.Contains(c.ConfiguredSecrets, "vision_key")
}
