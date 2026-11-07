package adapter

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"wechat-bot/internal/apphost"
)

// The documented endpoint is deliberately kept separate from search. It does
// not require, and must never receive, the owner search credential.
const (
	playerEndpoint         = "https://open.douyin.com/api/douyin/v1/video/get_iframe_by_video"
	maxPlayerResponseBytes = 64 << 10
	maxPlayerTitleRunes    = 512
	maxPlayerDimension     = 10000
)

// playerEntry is deliberately small. An official iframe establishes only an
// embed entry point; it is not evidence that the video has been played or read.
type playerEntry struct {
	URL    string
	Title  string
	Width  int
	Height int
}

type playerResponse struct {
	ErrNo  *int   `json:"err_no"`
	ErrMsg string `json:"err_msg"`
	Data   *struct {
		IFrameCode  string `json:"iframe_code"`
		VideoTitle  string `json:"video_title"`
		VideoWidth  int    `json:"video_width"`
		VideoHeight int    `json:"video_height"`
	} `json:"data"`
}

// fetchPlayer resolves an official public-video iframe into a canonical entry
// URL. It intentionally uses no cookies, no access token, and no redirects.
// Endpoint contract: https://developer.open-douyin.com/docs/resource/zh-CN/dop/develop/openapi/video-management/douyin/iframe-player/get-iframe-by-video
func (p *SearchApp) fetchPlayer(ctx context.Context, videoID string) (playerEntry, error) {
	if !decimalID.MatchString(videoID) {
		return playerEntry{}, apphost.ErrObservationInvalid
	}
	endpoint, err := url.Parse(playerEndpoint)
	if err != nil {
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	endpoint.RawQuery = url.Values{"video_id": {videoID}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Agent-home-station/1.0 douyin-player")

	// Clone instead of changing the shared search client: callers can never
	// inherit a cookie jar or a redirect policy from a test/custom client.
	client := *p.client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return playerEntry{}, ctx.Err()
		}
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPlayerResponseBytes+1))
	if err != nil || len(raw) > maxPlayerResponseBytes {
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	var decoded playerResponse
	if json.Unmarshal(raw, &decoded) != nil || decoded.ErrNo == nil {
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	if *decoded.ErrNo != 0 {
		if *decoded.ErrNo == 28003004 { // documented: the video is not public
			return playerEntry{}, apphost.ErrObservationContentUnavailable
		}
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	if decoded.Data == nil {
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	title := sanitizePlayerTitle(decoded.Data.VideoTitle)
	iframeURL, ok := strictIFrameURL(decoded.Data.IFrameCode, videoID)
	if !ok {
		return playerEntry{}, apphost.ErrObservationUpstreamUnavailable
	}
	width, height := decoded.Data.VideoWidth, decoded.Data.VideoHeight
	// Dimensions are descriptive metadata only. A missing or malformed pair
	// must not prevent us from returning the independently verified URL.
	if width < 1 || width > maxPlayerDimension || height < 1 || height > maxPlayerDimension {
		width, height = 0, 0
	}
	return playerEntry{URL: iframeURL, Title: title, Width: width, Height: height}, nil
}

func sanitizePlayerTitle(title string) string {
	title = strings.TrimSpace(title)
	if utf8RuneCount(title) > maxPlayerTitleRunes {
		title = string([]rune(title)[:maxPlayerTitleRunes])
	}
	var builder strings.Builder
	for _, r := range title {
		if unicode.IsControl(r) {
			builder.WriteByte(' ')
			continue
		}
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String())
}

func utf8RuneCount(value string) int {
	count := 0
	for range value {
		count++
	}
	return count
}

// strictIFrameURL accepts one empty official iframe only. We parse the small
// HTML subset rather than exposing upstream HTML to the rest of the program.
func strictIFrameURL(code, videoID string) (string, bool) {
	code = strings.TrimSpace(code)
	if len(code) < len("<iframe></iframe>") || !hasASCIIPrefixFold(code, "<iframe") {
		return "", false
	}
	position := len("<iframe")
	if position >= len(code) || !isHTMLSpace(code[position]) {
		return "", false
	}
	src, next, ok := parseIFrameAttributes(code, position)
	remainder := strings.TrimSpace(code[next:])
	if !ok || !hasASCIIPrefixFold(remainder, "</iframe>") || strings.TrimSpace(remainder[len("</iframe>"):]) != "" {
		return "", false
	}
	src = html.UnescapeString(src)
	parsed, err := url.Parse(src)
	if err != nil || strings.Contains(src, "#") || parsed.Scheme != "https" || parsed.Host != "open.douyin.com" || parsed.Path != "/player/video" || parsed.RawPath != "" || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.User != nil || parsed.Port() != "" {
		return "", false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", false
	}
	if len(query) != 2 || len(query["vid"]) != 1 || query.Get("vid") != videoID || len(query["autoplay"]) != 1 || query.Get("autoplay") != "0" {
		return "", false
	}
	return "https://open.douyin.com/player/video?" + url.Values{"autoplay": {"0"}, "vid": {videoID}}.Encode(), true
}

func parseIFrameAttributes(code string, position int) (string, int, bool) {
	var src string
	haveSrc := false
	for {
		for position < len(code) && isHTMLSpace(code[position]) {
			position++
		}
		if position >= len(code) {
			return "", 0, false
		}
		if code[position] == '>' {
			return src, position + 1, haveSrc
		}
		start := position
		for position < len(code) && isAttributeNameByte(code[position]) {
			position++
		}
		if start == position {
			return "", 0, false
		}
		name := strings.ToLower(code[start:position])
		for position < len(code) && isHTMLSpace(code[position]) {
			position++
		}
		if position >= len(code) || code[position] != '=' {
			if name == "src" {
				return "", 0, false
			}
			// Official examples may include boolean attributes such as
			// allowfullscreen. They do not affect the only value we retain.
			continue
		}
		position++
		for position < len(code) && isHTMLSpace(code[position]) {
			position++
		}
		if position >= len(code) || (code[position] != '\'' && code[position] != '"') {
			return "", 0, false
		}
		quote := code[position]
		position++
		valueStart := position
		for position < len(code) && code[position] != quote {
			if code[position] == '<' || code[position] == '>' || code[position] == '\r' || code[position] == '\n' {
				return "", 0, false
			}
			position++
		}
		if position >= len(code) {
			return "", 0, false
		}
		value := code[valueStart:position]
		position++
		if name == "src" {
			if haveSrc || value == "" {
				return "", 0, false
			}
			haveSrc, src = true, value
		}
	}
}

func hasASCIIPrefixFold(value, prefix string) bool {
	if len(value) < len(prefix) {
		return false
	}
	return strings.EqualFold(value[:len(prefix)], prefix)
}

func isHTMLSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f'
}

func isAttributeNameByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '-' || value == ':' || value == '_'
}
