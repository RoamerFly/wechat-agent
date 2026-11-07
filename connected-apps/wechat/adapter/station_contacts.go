package adapter

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

const StationContactsToolName = "wechat_contacts"

func StationContactsTool() mcptools.Tool {
	return tool(StationContactsToolName, "Read the local WeChat contacts snapshot for an owner-authorized connection. Call with {} to discover allowed connection IDs, then pass connection_id and optional search, limit or cursor. Default limit 50, native maximum 200 per page; pass the returned next_cursor as cursor to continue. Contact names are external records. This tool does not refresh the remote directory or send messages.", `{"type":"object","properties":{"connection_id":{"type":"string","maxLength":128},"search":{"type":"string","maxLength":256},"cursor":{"type":"string","maxLength":11000},"limit":{"type":"integer","minimum":1}},"additionalProperties":false}`)
}

type StationContact struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// ProjectStationDirectory keeps native profile locations and protocol fields in
// the host. The native coverage object includes database paths and generations.
func ProjectStationDirectory(page linuxnative.CatalogPage, groups bool) ([]StationContact, *bool, error) {
	items := make([]StationContact, 0, len(page.Items))
	seen := make(map[string]bool, len(page.Items))
	for _, row := range page.Items {
		id, _ := row["id"].(string)
		username, _ := row["username"].(string)
		if groups {
			username, _ = row["roomId"].(string)
			if username != id || !strings.HasSuffix(id, "@chatroom") {
				return nil, nil, errors.New("wechat_group_identity_invalid")
			}
		}
		if id == "" || len(id) > 256 || !utf8.ValidString(id) || strings.ContainsFunc(id, func(r rune) bool {
			return unicode.IsControl(r) || unicode.IsSpace(r)
		}) || username != "" && username != id || seen[id] {
			return nil, nil, errors.New("wechat_contact_identity_invalid")
		}
		seen[id] = true
		name := ""
		for _, field := range []string{"remark", "displayName", "nickname"} {
			if value, ok := row[field].(string); ok {
				if name = identity.Clean(value); name != "" {
					break
				}
			}
		}
		if name == "" {
			name = id
		}
		items = append(items, StationContact{ID: id, DisplayName: name})
	}
	if coverage, ok := page.Coverage.(map[string]any); ok {
		if partial, ok := coverage["partial"].(bool); ok {
			return items, &partial, nil
		}
	}
	return items, nil, nil
}
