package admin

import (
	"encoding/json"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
)

// Zero duration means the remainder, zero segment/timeout select defaults.
// Only WeChat's actual 60-second voice protocol maximum is a ceiling.
type voiceConversion wechatapp.VoiceConversion

func (c *voiceConversion) effective() voiceConversion {
	return voiceConversion((*wechatapp.VoiceConversion)(c).Effective())
}
func (c *voiceConversion) validate(kind string) error {
	if err := (*wechatapp.VoiceConversion)(c).Validate(kind); err != nil {
		return toolInputError(err.Error())
	}
	return nil
}

func (c *voiceConversion) formatKey() string {
	v := c.effective()
	v.TimeoutSeconds = 0 // Execution budget cannot change output identity.
	b, _ := json.Marshal(v)
	return "voice-" + agentID("conversion", string(b))
}

func sameVoiceConversion(a, b *voiceConversion) bool { return a.formatKey() == b.formatKey() }

func withVoiceConversion(schema string) string { return wechatapp.WithVoiceConversion(schema) }
