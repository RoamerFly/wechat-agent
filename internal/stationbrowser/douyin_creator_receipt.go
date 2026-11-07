//go:build windows

package stationbrowser

import (
	"encoding/json"
	"strings"
	"time"
)

func validCreatorPublicationQuery(q CreatorPublicationQuery) bool {
	_, err := time.Parse(time.RFC3339Nano, q.CreatedAt)
	return err == nil && strings.TrimSpace(q.Title+q.Body) != "" &&
		(q.Visibility == "private" || q.Visibility == "public" || q.Visibility == "friends")
}

// creatorManageReceipt reads only the creator page's own work-list response.
// It never uses a toast or a guessed ID. Unknown, mismatched, duplicated or
// incomplete records stay unknown; no caller may turn that into another post.
func creatorManageReceipt(raw []byte, query *CreatorPublicationQuery, knownID string) CreatorPublicationResult {
	unknown := CreatorPublicationResult{Stage: "awaiting_receipt", Submitted: true}
	if len(raw) > 256<<10 || (knownID == "" && (query == nil || !validCreatorPublicationQuery(*query))) {
		return unknown
	}
	var page struct {
		StatusCode *int `json:"status_code"`
		Works      []struct {
			ID          string `json:"aweme_id"`
			Title       string `json:"item_title"`
			Caption     string `json:"caption"`
			Description string `json:"desc"`
			Created     int64  `json:"create_time"`
			Status      struct {
				ID            string `json:"aweme_id"`
				Private       *bool  `json:"is_private"`
				PrivateStatus *int   `json:"private_status"`
				Reviewing     *bool  `json:"in_reviewing"`
				Deleted       *bool  `json:"is_delete"`
				Prohibited    *bool  `json:"is_prohibited"`
				SelfSee       *bool  `json:"self_see"`
			} `json:"status"`
		} `json:"aweme_list"`
	}
	if json.Unmarshal(raw, &page) != nil || page.StatusCode == nil || *page.StatusCode != 0 {
		return unknown
	}
	text := func(s string) string {
		return strings.Join(strings.Fields(strings.NewReplacer("\u200b", "", "\ufeff", "").Replace(s)), " ")
	}
	found := map[string]CreatorPublicationResult{}
	for _, w := range page.Works {
		// JSON number-to-float conversion can destroy a 19-digit work number.
		// Use the canonical string ID, never the legacy numeric item_id alias.
		if w.ID == "" || w.Status.ID != w.ID || !douyinVideoID.MatchString(w.ID) || (knownID != "" && w.ID != knownID) {
			continue
		}
		st := w.Status
		if st.Deleted == nil || *st.Deleted || st.Prohibited == nil || *st.Prohibited || st.Reviewing == nil || st.Private == nil {
			continue
		}
		if query != nil {
			created, err := time.Parse(time.RFC3339Nano, query.CreatedAt)
			if err != nil || w.Created < created.Unix() || w.Created > time.Now().Add(time.Minute).Unix() ||
				text(w.Title) != text(query.Title) || text(w.Caption) != text(query.Body) ||
				text(w.Description) != text(query.Title+" "+query.Body) {
				continue
			}
			if st.PrivateStatus == nil || (query.Visibility == "private" && (!*st.Private || *st.PrivateStatus != 1)) ||
				(query.Visibility == "public" && (*st.Private || *st.PrivateStatus != 0)) || query.Visibility == "friends" {
				continue
			}
		}
		published := !*st.Reviewing && (!*st.Private || (st.SelfSee != nil && *st.SelfSee))
		stage := "accepted"
		if published {
			stage = "published"
		}
		found[w.ID] = CreatorPublicationResult{Stage: stage, RemoteID: w.ID, Submitted: true, Published: published}
	}
	if len(found) != 1 {
		return unknown
	}
	for _, r := range found {
		return r
	}
	return unknown
}
