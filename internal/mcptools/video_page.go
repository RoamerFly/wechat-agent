package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var biliVideoPath = regexp.MustCompile(`^/video/(BV[A-Za-z0-9]{10})/?$`)
var biliShortPath = regexp.MustCompile(`^/[A-Za-z0-9]{3,32}/?$`)
var biliCanonicalBV = regexp.MustCompile(`^BV[0-9A-Za-z]{10}$`)

type publicVideoRefererKey struct{}
type PublicVideo struct {
	LinkOnly                    bool
	Data                        []byte
	ContentType, Title, PageURL string
	DurationMS                  int64
}
type biliPage struct {
	Video struct {
		BVID     string `json:"bvid"`
		CID      int64  `json:"cid"`
		Title    string `json:"title"`
		Duration int64  `json:"duration"`
		Pages    []struct {
			CID      int64  `json:"cid"`
			Part     string `json:"part"`
			Duration int64  `json:"duration"`
		} `json:"pages"`
	} `json:"videoData"`
}

func IsPublicVideoPage(raw string) bool {
	u, e := url.Parse(raw)
	return kuaishouPhotoID(raw) != "" || e == nil && ((u.Hostname() == "www.bilibili.com" || u.Hostname() == "bilibili.com") && biliVideoPath.MatchString(u.Path) || u.Hostname() == "b23.tv" && biliShortPath.MatchString(u.Path))
}
func parseBiliPage(body []byte, bvid string, part int) (biliPage, error) {
	var out biliPage
	marker := []byte("window.__INITIAL_STATE__")
	i := bytes.Index(body, marker)
	if i < 0 {
		return out, &WebError{Code: "video_page_metadata_unavailable", Stage: "video_metadata"}
	}
	data := bytes.TrimSpace(body[i+len(marker):])
	if len(data) == 0 || data[0] != '=' {
		return out, &WebError{Code: "video_page_metadata_unavailable", Stage: "video_metadata"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data[1:]))
	if decoder.Decode(&out) != nil {
		return out, &WebError{Code: "video_page_metadata_unavailable", Stage: "video_metadata"}
	}
	return selectBiliPart(out, bvid, part)
}

func selectBiliPart(out biliPage, bvid string, part int) (biliPage, error) {
	if out.Video.BVID != bvid || out.Video.CID <= 0 || strings.TrimSpace(out.Video.Title) == "" || len(out.Video.Title) > 1024 || part < 1 {
		return out, &WebError{Code: "video_page_metadata_unavailable", Stage: "video_metadata"}
	}
	if part > 1 {
		if part > len(out.Video.Pages) || out.Video.Pages[part-1].CID <= 0 {
			return out, &WebError{Code: "video_part_unavailable", Stage: "video_metadata"}
		}
		out.Video.CID = out.Video.Pages[part-1].CID
		out.Video.Duration = out.Video.Pages[part-1].Duration
		out.Video.Title += " · " + out.Video.Pages[part-1].Part
	}
	return out, nil
}
func parseBiliProgressive(body []byte, limit int64, sampling bool) (string, int64, error) {
	var result struct {
		Code *int `json:"code"`
		Data struct {
			Duration  int64           `json:"timelength"`
			IsPreview json.RawMessage `json:"is_preview"`
			Format    string          `json:"format"`
			Durl      []struct {
				URL  string `json:"url"`
				Size int64  `json:"size"`
			} `json:"durl"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil {
		return "", 0, &WebError{Code: "video_page_metadata_unavailable", Stage: "video_playback"}
	}
	if sampling && result.Code != nil && (*result.Code == -412 || *result.Code == -352) {
		return "", 0, &WebError{Code: "http_status", Status: 412, Stage: "video_playback"}
	}
	preview := string(bytes.TrimSpace(result.Data.IsPreview))
	if result.Code != nil && *result.Code != 0 || sampling && result.Code == nil ||
		preview == "true" || preview == "1" || sampling && preview != "" && preview != "false" && preview != "0" && preview != "null" {
		return "", 0, &WebError{Code: "video_public_playback_unavailable", Stage: "video_playback"}
	}
	if len(result.Data.Durl) != 1 || sampling && !strings.EqualFold(result.Data.Format, "mp4") {
		return "", 0, &WebError{Code: "video_progressive_unavailable", Stage: "video_playback"}
	}
	if result.Data.Duration <= 0 || !sampling && result.Data.Duration > 10*60*1000 {
		return "", 0, &WebError{Code: "video_duration_limit", Stage: "video_playback"}
	}
	file := result.Data.Durl[0]
	if sampling && file.Size <= 0 {
		return "", 0, &WebError{Code: "video_public_playback_unavailable", Stage: "video_playback"}
	}
	if file.Size > limit {
		return "", 0, &WebError{Code: "response_too_large", Stage: "video_playback"}
	}
	u, e := url.Parse(file.URL)
	if e != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".bilivideo.com") || validPublicURL(file.URL) != nil {
		return "", 0, &WebError{Code: "video_public_playback_unavailable", Stage: "video_playback"}
	}
	return file.URL, result.Data.Duration, nil
}

// Resolve only ordinary, anonymously playable BV pages. No cookies, account
// state, paid formats, DRM keys, JavaScript execution or arbitrary API calls.
func DownloadPublicVideoPage(ctx context.Context, raw string, limit int64) (PublicVideo, error) {
	return downloadPublicVideoPage(ctx, raw, limit, false, false)
}

// Audio extraction uses the same anonymously playable progressive file;
// it never supplies cookies, accesses paid streams or guesses audio endpoints.
func DownloadPublicVideoAudio(ctx context.Context, raw string, limit int64) (PublicVideo, error) {
	return downloadPublicVideoPage(ctx, raw, limit, true, false)
}

// Receiving and understanding a long video still requires actual content;
// the one-minute link-only rule applies to delivery, never to observation.
func DownloadPublicVideoObservation(ctx context.Context, raw string, limit int64) (PublicVideo, error) {
	return downloadPublicVideoPage(ctx, raw, limit, true, false)
}

// GetPublicBilibiliVideo resolves one exact, anonymously playable BV into a
// bounded progressive file. It deliberately rejects multi-part works rather
// than silently sampling their first part. The returned value never exposes a
// signed CDN URL.
func GetPublicBilibiliVideo(ctx context.Context, bvid string, limit int64) (PublicVideo, error) {
	if !biliCanonicalBV.MatchString(bvid) || limit <= 0 || limit > 32<<20 {
		return PublicVideo{}, &WebError{Code: "video_public_playback_unavailable", Stage: "video_playback"}
	}
	return downloadPublicVideoPage(ctx, "https://www.bilibili.com/video/"+bvid+"/", limit, true, true)
}

func downloadPublicVideoPage(ctx context.Context, raw string, limit int64, audio, singlePart bool) (PublicVideo, error) {
	if kuaishouPhotoID(raw) != "" {
		return downloadKuaishouVideo(ctx, raw, limit, audio)
	}
	var out PublicVideo
	if !IsPublicVideoPage(raw) || validPublicURL(raw) != nil {
		return out, &WebError{Code: "video_page_unsupported"}
	}
	u, _ := url.Parse(raw)
	metaCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var prefetched []byte
	if u.Hostname() == "b23.tv" {
		body, _, final, e := publicRead(metaCtx, raw, maxBody)
		if e != nil {
			return out, e
		}
		u, e = url.Parse(final)
		if e != nil || !(u.Hostname() == "www.bilibili.com" || u.Hostname() == "bilibili.com") || !biliVideoPath.MatchString(u.Path) {
			return out, &WebError{Code: "video_page_unsupported"}
		}
		prefetched = body
	}
	match := biliVideoPath.FindStringSubmatch(u.Path)
	part := 1
	if p := u.Query().Get("p"); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 {
			return out, &WebError{Code: "video_part_unavailable"}
		}
		part = n
	}
	page := "https://www.bilibili.com/video/" + match[1] + "/"
	if part > 1 {
		page += "?p=" + strconv.Itoa(part)
	}
	body := prefetched
	var err error
	if body == nil {
		body, _, _, err = publicRead(metaCtx, page, maxBody)
	}
	if err != nil {
		return out, err
	}
	state, err := parseBiliPage(body, match[1], part)
	// Some public share pages contain only the player shell. Resolve the
	// same BV through the public metadata API before selecting its exact CID.
	if err != nil && !bytes.Contains(body, []byte("window.__INITIAL_STATE__")) {
		viewURL := "https://api.bilibili.com/x/web-interface/view?bvid=" + match[1]
		raw, _, final, readErr := publicRead(metaCtx, viewURL, maxBody)
		if readErr != nil {
			return out, readErr
		}
		var view struct {
			Code *int            `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		var exact biliPage
		if final != viewURL || json.Unmarshal(raw, &view) != nil || view.Code == nil || *view.Code != 0 || json.Unmarshal(view.Data, &exact.Video) != nil {
			return out, &WebError{Code: "video_page_metadata_unavailable", Stage: "video_metadata"}
		}
		state, err = selectBiliPart(exact, match[1], part)
	}
	if err != nil {
		return out, err
	}
	// Playback sampling must be able to prove which media part supplied the
	// bytes. A missing pages list is not proof of a single-part work, so reject
	// it together with ordinary multi-part works.
	if singlePart && (len(state.Video.Pages) != 1 || state.Video.Pages[0].CID != state.Video.CID || state.Video.Pages[0].Duration <= 0) {
		return out, &WebError{Code: "video_public_playback_unavailable", Stage: "video_playback"}
	}
	if !audio && state.Video.Duration > 60 {
		return PublicVideo{LinkOnly: true, Title: state.Video.Title, PageURL: page, DurationMS: state.Video.Duration * 1000}, nil
	}
	quality, fnval := "32", "0"
	if singlePart {
		quality, fnval = "16", "1"
	}
	api := "https://api.bilibili.com/x/player/playurl?" + url.Values{"bvid": {match[1]}, "cid": {strconv.FormatInt(state.Video.CID, 10)}, "qn": {quality}, "fnval": {fnval}, "fourk": {"0"}}.Encode()
	body, _, _, err = publicRead(metaCtx, api, maxBody)
	if err != nil {
		return out, err
	}
	var length struct {
		Code int `json:"code"`
		Data struct {
			DurationMS int64 `json:"timelength"`
		} `json:"data"`
	}
	if !audio && json.Unmarshal(body, &length) == nil && length.Code == 0 && length.Data.DurationMS > 60000 {
		return PublicVideo{LinkOnly: true, Title: state.Video.Title, PageURL: page, DurationMS: length.Data.DurationMS}, nil
	}
	direct, duration, err := parseBiliProgressive(body, limit, singlePart)
	if err != nil {
		return out, err
	}
	downloadCtx := context.WithValue(ctx, publicVideoRefererKey{}, true)
	data, typ, err := GetPublicMedia(downloadCtx, direct, limit)
	if err != nil {
		return out, err
	}
	typ, err = ValidateMediaType("video", typ, data)
	if err != nil {
		return out, err
	}
	if singlePart && typ != "video/mp4" {
		return out, &WebError{Code: "media_type_mismatch", Stage: "video_playback"}
	}
	return PublicVideo{Data: data, ContentType: typ, Title: state.Video.Title, PageURL: page, DurationMS: duration}, nil
}
