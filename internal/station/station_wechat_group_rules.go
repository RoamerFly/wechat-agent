package station

import (
	"strings"
	"time"
	"unicode/utf8"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type stationWechatGroupRule struct {
	Note                    string    `json:"note,omitempty"`
	SummarizeSince          time.Time `json:"summarize_since,omitzero"`
	Mode                    string    `json:"mode,omitempty"`
	Prefix                  string    `json:"prefix,omitempty"`
	Persona                 string    `json:"persona,omitempty"`
	ForceReplyWhenMentioned bool      `json:"force_reply_when_mentioned,omitempty"`
	// ReplySince is an authorization epoch for replies. It prevents inputs
	// queued before an explicit reply-policy change from being revived later.
	ReplySince time.Time `json:"reply_since,omitzero"`
}

// groupParticipation derives engine permissions only from currently granted
// groups. Bot rules retain each group's own trigger mode within that engine.
func (policy stationWechatPolicy) groupParticipation(config *companion.Config) ([]string, map[string]bot.GroupRule) {
	threads := make([]string, 0, len(policy.Grants))
	rules := make(map[string]bot.GroupRule)
	config.Permissions.ObserveGroups, config.Permissions.AutonomousGroups = false, false
	for thread, granted := range policy.Grants {
		if granted.IsZero() {
			continue
		}
		threads = append(threads, thread)
		if validStationGroup(thread) {
			rules[thread] = policy.GroupRules[thread].replyRule()
			if rules[thread].Mode == "social" {
				config.Permissions.ObserveGroups, config.Permissions.AutonomousGroups = true, true
			}
		}
	}
	return threads, rules
}

func (rule stationWechatGroupRule) replyRule() bot.GroupRule {
	mode := rule.Mode
	if mode == "" {
		mode = "mention"
	}
	return bot.GroupRule{Mode: mode, Prefix: rule.Prefix, Persona: rule.Persona, ForceReplyWhenMentioned: rule.ForceReplyWhenMentioned}
}

func (rule stationWechatGroupRule) hasReplySettings() bool {
	return rule.Mode == "all" || rule.Mode == "social" || rule.Prefix != "" || rule.Persona != "" || rule.ForceReplyWhenMentioned
}

func validStationGroupReplyValues(mode, prefix, persona string) bool {
	if mode != "" && mode != "mention" && mode != "all" && mode != "social" {
		return false
	}
	return len(prefix) <= 64 && utf8.ValidString(prefix) &&
		!strings.ContainsAny(prefix, "\r\n\x00") &&
		len(persona) <= 4000 && utf8.ValidString(persona)
}

func validStationGroupReplySettings(settings apphost.ConnectionGroupReplySettings) bool {
	return validStationGroupReplyValues(settings.Mode, settings.Prefix, settings.Persona)
}

// setReplySettings validates and normalizes a requested reply policy. Empty
// and "mention" are both persisted as the canonical default empty Mode. It
// advances ReplySince only when the effective bot rule changes, so callers can
// use it during a migration without reviving previously queued messages.
func (rule *stationWechatGroupRule) setReplySettings(settings apphost.ConnectionGroupReplySettings, now time.Time) error {
	if !validStationGroupReplySettings(settings) {
		return apphost.ErrInvalidConnectionPolicy
	}
	previous := rule.replyRule()
	rule.Mode, rule.Prefix, rule.Persona = "", settings.Prefix, settings.Persona
	rule.ForceReplyWhenMentioned = settings.ForceReplyWhenMentioned
	if settings.Mode == "all" || settings.Mode == "social" {
		rule.Mode = settings.Mode
	}
	if rule.replyRule() != previous {
		rule.ReplySince = now
	}
	return nil
}

func validStationGroupRule(thread string, rule stationWechatGroupRule, replies, attachments map[string]time.Time) bool {
	parent := replies[thread]
	if !validStationGroup(thread) || parent.IsZero() || len(rule.Note) > 4000 || !utf8.ValidString(rule.Note) ||
		!validStationGroupReplyValues(rule.Mode, rule.Prefix, rule.Persona) || (!rule.ReplySince.IsZero() && rule.ReplySince.Before(parent)) {
		return false
	}
	if rule.SummarizeSince.IsZero() {
		return true
	}
	media := attachments[thread]
	return !media.IsZero() && !rule.SummarizeSince.Before(media) && !rule.SummarizeSince.Before(parent)
}

func stationGroupRules(previous map[string]stationWechatGroupRule, requested *[]apphost.ConnectionGroupRule, replies, attachments map[string]time.Time, now time.Time) (map[string]stationWechatGroupRule, error) {
	result := make(map[string]stationWechatGroupRule)
	if requested == nil {
		for thread, rule := range previous {
			if attachments[thread].IsZero() {
				rule.SummarizeSince = time.Time{}
			}
			if validStationGroupRule(thread, rule, replies, attachments) && (rule.Note != "" || !rule.SummarizeSince.IsZero() || rule.hasReplySettings() || !rule.ReplySince.IsZero()) {
				result[thread] = rule
			}
		}
		return result, nil
	}
	if len(*requested) > 100 {
		return nil, apphost.ErrInvalidConnectionPolicy
	}
	seen := make(map[string]bool, len(*requested))
	for _, row := range *requested {
		// Keep existing reply settings when an older client omits the field.
		rule := previous[row.ThreadID]
		rule.Note = strings.TrimSpace(row.Note)
		if row.ReplySettings != nil {
			if err := rule.setReplySettings(*row.ReplySettings, now); err != nil {
				return nil, err
			}
		}
		if row.SummarizeShares {
			if rule.SummarizeSince.IsZero() {
				rule.SummarizeSince = now
			}
		} else {
			rule.SummarizeSince = time.Time{}
		}
		if seen[row.ThreadID] || !validStationGroupRule(row.ThreadID, rule, replies, attachments) {
			return nil, apphost.ErrInvalidConnectionPolicy
		}
		seen[row.ThreadID] = true
		if rule.Note != "" || !rule.SummarizeSince.IsZero() || rule.hasReplySettings() || !rule.ReplySince.IsZero() {
			result[row.ThreadID] = rule
		}
	}
	return result, nil
}

func (p stationWechatPolicy) groupShareAllowed(thread, timestamp string) bool {
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	rule := p.GroupRules[thread]
	return err == nil && !rule.SummarizeSince.IsZero() && !p.Grants[thread].IsZero() &&
		!p.AttachmentGrants[thread].IsZero() && !at.Before(rule.SummarizeSince)
}

func (p stationWechatPolicy) groupShareCandidate(account string, m linuxnative.Message) bool {
	return m.AccountID == account && m.Direction == "incoming" && p.groupShareAllowed(m.ThreadID, m.CreatedAt) &&
		wechatapp.IsChannelsShare(m)
}
