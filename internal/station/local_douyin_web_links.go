package station

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/stationbrowser"
	"wechat-bot/internal/workspacefs"
)

type localDouyinWebTarget struct{ url, videoID string }
type localDouyinWebVideoRef struct {
	Snapshot string `json:"snapshot"`
	VideoID  string `json:"video"`
}
type localDouyinWebLinkProof struct {
	Version   int      `json:"schema_version"`
	Grant     string   `json:"grant"`
	Snapshot  string   `json:"snapshot"`
	SourceURL string   `json:"source_url"`
	Videos    []string `json:"videos"`
}

func localDouyinWebFollowUpID(snapshot, video string) string {
	raw, _ := json.Marshal(localDouyinWebVideoRef{Snapshot: snapshot, VideoID: video})
	return douyinapp.WebVideoContentPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func (p *localDouyinWebObservation) snapshotDirectory(a localDouyinWebAccess) string {
	return filepath.Join(p.home.dataDir, "apps", "douyin", "web-snapshots", a.record.FishID, a.version)
}

// A guessed video ID is insufficient. The exact video must be present in an
// immutable, host-created home-page link index under the current owner grant.
func (p *localDouyinWebObservation) resolveTarget(a localDouyinWebAccess, id string) (localDouyinWebTarget, bool) {
	if id == douyinapp.WebReadContentID {
		return localDouyinWebTarget{url: localDouyinWebHomeURL}, true
	}
	if localDouyinWebScope(a.record) != "home_and_videos" || !douyinapp.IsWebReadContentID(id) || !strings.HasPrefix(id, douyinapp.WebVideoContentPrefix) {
		return localDouyinWebTarget{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, douyinapp.WebVideoContentPrefix))
	var ref localDouyinWebVideoRef
	if err != nil || len(raw) > 512 || strictLocalMemoryDecode(raw, &ref) != nil || !validLocalDouyinWebID(ref.Snapshot) {
		return localDouyinWebTarget{}, false
	}
	url, err := stationbrowser.DouyinVideoURL(ref.VideoID)
	if err != nil {
		return localDouyinWebTarget{}, false
	}
	root, err := workspacefs.OpenPath(p.snapshotDirectory(a))
	if err != nil {
		return localDouyinWebTarget{}, false
	}
	defer root.Close()
	f, err := workspacefs.OpenRegular(root, ref.Snapshot+".links.json")
	if err != nil {
		return localDouyinWebTarget{}, false
	}
	raw, err = io.ReadAll(io.LimitReader(f, localDouyinWebSnapshotMax+1))
	f.Close()
	var proof localDouyinWebLinkProof
	if err != nil || len(raw) > localDouyinWebSnapshotMax || json.Unmarshal(raw, &proof) != nil || proof.Version != 1 || proof.Grant != a.version || proof.Snapshot != ref.Snapshot || proof.SourceURL != localDouyinWebHomeURL {
		return localDouyinWebTarget{}, false
	}
	for _, video := range proof.Videos {
		if video == ref.VideoID {
			return localDouyinWebTarget{url: url, videoID: video}, true
		}
	}
	return localDouyinWebTarget{}, false
}

func saveLocalDouyinWebLinkProof(dir string, snapshot localDouyinWebSnapshot) error {
	if snapshot.ContentID != douyinapp.WebReadContentID || len(snapshot.Links) == 0 {
		return nil
	}
	proof := localDouyinWebLinkProof{Version: 1, Grant: snapshot.Grant, Snapshot: snapshot.ID, SourceURL: localDouyinWebHomeURL}
	seen := map[string]bool{}
	for _, link := range snapshot.Links {
		if _, err := stationbrowser.DouyinVideoURL(link.VideoID); err != nil {
			return apphost.ErrObservationContentUnavailable
		}
		if !seen[link.VideoID] {
			proof.Videos = append(proof.Videos, link.VideoID)
			seen[link.VideoID] = true
		}
	}
	raw, err := json.Marshal(proof)
	if err != nil || len(raw) > localDouyinWebSnapshotMax {
		return apphost.ErrObservationPageTextSize
	}
	if err := hostfile.WritePrivate(filepath.Join(dir, snapshot.ID+".links.json"), raw); err != nil {
		return apphost.ErrObservationWebStorageUnavailable
	}
	return nil
}

type localDouyinWebSegment struct{ text, followUp string }

// Keep discovered targets near the front so selecting one does not require
// injecting the entire home page into the model. Both lists and body paginate
// within the caller's same character budget; neither is silently shortened.
func localDouyinWebSegments(s localDouyinWebSnapshot) []localDouyinWebSegment {
	segments := []localDouyinWebSegment{}
	if s.Title != "" {
		segments = append(segments, localDouyinWebSegment{text: "页面标题：" + s.Title + "\n"})
	}
	if len(s.Links) > 0 {
		segments = append(segments, localDouyinWebSegment{text: "[首页实际可见的视频链接]\n"})
		seen := map[string]bool{}
		for _, link := range s.Links {
			if seen[link.VideoID] {
				continue
			}
			seen[link.VideoID] = true
			label := strings.TrimSpace(link.Text)
			if label == "" {
				label = "视频 " + link.VideoID
			}
			segments = append(segments, localDouyinWebSegment{text: label + "\n", followUp: localDouyinWebFollowUpID(s.ID, link.VideoID)})
		}
		segments = append(segments, localDouyinWebSegment{text: "[页面实际可见文字]\n"})
	}
	return append(segments, localDouyinWebSegment{text: s.Text})
}

func localDouyinWebPage(s localDouyinWebSnapshot, offset, limit int) (apphost.ObservationPage, error) {
	segments := localDouyinWebSegments(s)
	total := 0
	for _, seg := range segments {
		total += len([]rune(seg.text))
	}
	if offset < 0 || offset >= total {
		return apphost.ObservationPage{}, apphost.ErrObservationWebSnapshotChanged
	}
	defaultPage := limit == 0
	if defaultPage {
		limit = 2048
	}
	end := total
	if limit < total-offset {
		end = offset + limit
	}
	// Source metadata is larger than a short anchor label. Default pages offer
	// eight candidates, then continue; an explicit character limit overrides
	// that starting selection rather than silently keeping an eight-link cap.
	if defaultPage {
		position, count := 0, 0
		for _, seg := range segments {
			next := position + len([]rune(seg.text))
			if seg.followUp != "" && next > offset && position < end {
				count++
				if count == 8 {
					end = min(end, next)
					break
				}
			}
			position = next
		}
	}
	page := apphost.ObservationPage{Complete: end == total}
	position := 0
	for _, seg := range segments {
		runes := []rune(seg.text)
		next := position + len(runes)
		start, stop := max(position, offset), min(next, end)
		if start < stop {
			text := string(runes[start-position : stop-position])
			if strings.TrimSpace(text) != "" {
				url := s.SourceURL
				if url == "" {
					url = localDouyinWebHomeURL
				}
				chunk := apphost.ObservationChunk{Position: start, Text: text, SourceID: s.ID, SourceURL: url}
				if seg.followUp != "" {
					chunk.FollowUpContentID = seg.followUp
					chunk.FollowUpField = apphost.ObservationPageText
				}
				page.Chunks = append(page.Chunks, chunk)
			}
		}
		position = next
		if position >= end {
			break
		}
	}
	page.Coverage = fmtLocalDouyinWebCoverage(offset, end, total, s.CapturedAt, len(s.Links) > 0)
	if !page.Complete {
		raw, _ := json.Marshal(localDouyinWebCursor{ID: s.ID, Offset: end})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func readLocalDouyinWebSnapshot(root *os.Root, cursor localDouyinWebCursor, grant, contentID string) (localDouyinWebSnapshot, error) {
	f, err := workspacefs.OpenRegular(root, cursor.ID+".json")
	if err != nil {
		return localDouyinWebSnapshot{}, apphost.ErrObservationWebSnapshotChanged
	}
	raw, err := io.ReadAll(io.LimitReader(f, localDouyinWebSnapshotMax+1))
	f.Close()
	var snapshot localDouyinWebSnapshot
	if err != nil || len(raw) > localDouyinWebSnapshotMax || json.Unmarshal(raw, &snapshot) != nil || snapshot.Version != 1 || snapshot.Grant != grant || snapshot.ID != cursor.ID {
		return snapshot, apphost.ErrObservationWebSnapshotChanged
	}
	if snapshot.ContentID == "" {
		snapshot.ContentID = douyinapp.WebReadContentID
	} // v1 home snapshots
	if snapshot.ContentID != contentID {
		return snapshot, apphost.ErrObservationWebSnapshotChanged
	}
	return snapshot, nil
}
