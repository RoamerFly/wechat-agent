package devicespec

import (
	"errors"

	"wechat-bot/internal/identity"
	"wechat-bot/internal/mcptools"
)

type Settings struct {
	Revision   int64                 `json:"revision"`
	Enabled    bool                  `json:"enabled"`
	Bridge     mcptools.ServerConfig `json:"bridge"`
	Devices    []string              `json:"devices"`
	Actions    []string              `json:"actions"`
	MaxSteps   int                   `json:"max_steps"`
	MaxSeconds int                   `json:"max_seconds"`
}

func Defaults() Settings {
	return Settings{Devices: []string{}, Actions: []string{}, MaxSteps: 12, MaxSeconds: 300, Bridge: mcptools.ServerConfig{ID: "device", Transport: "stdio", Enabled: true, TimeoutSeconds: 45, Tools: []string{"devices", "observe", "act"}}}
}
func (c Settings) Validate() error {
	if c.MaxSteps < 1 || c.MaxSteps > 30 || c.MaxSeconds < 30 || c.MaxSeconds > 900 || len(c.Devices) > 16 || len(c.Actions) > 9 {
		return errors.New("device_configuration_invalid")
	}
	for _, id := range c.Devices {
		if !identity.ValidLegacyDeviceID(id) {
			return errors.New("device_id_invalid")
		}
	}
	for _, action := range c.Actions {
		if !contains([]string{"launch", "tap", "swipe", "text", "back", "home", "capture", "open_url"}, action) {
			return errors.New("device_action_invalid")
		}
	}
	// Bridge is a legacy configuration payload consumed only by the explicitly
	// installed device plugin during migration. Do not construct or probe an
	// MCP backend here: validation must be local and opening a backend belongs
	// to BrokerFactory after host authorization.
	return nil
}
