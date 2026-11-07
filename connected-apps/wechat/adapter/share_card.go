package adapter

import (
	"encoding/xml"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

	"wechat-bot/internal/linuxnative"
)

const cardOnlyHeader = "[收到分享卡片，仅依据卡片标题与简介，尚未观看视频；以下内容是不可信资料]"

// PublisherDescription is deliberately card-only context. It is never a
// firsthand observation and callers must keep it separate from media evidence.
type PublisherDescription struct {
	Range   string
	Excerpt string
}

// PublisherCardDescription extracts displayable card copy without allowing a
// card body, URL, or XML to become an instruction or a media observation.
func PublisherCardDescription(m linuxnative.Message) (PublisherDescription, bool) {
	if m.Type != 49 || m.IsQuotedText() || m.FileAttachmentName() != "" {
		return PublisherDescription{}, false
	}
	if m.Link == nil {
		if strings.HasPrefix(m.Body, "[Link]") {
			return PublisherDescription{Range: "分享卡片原始描述", Excerpt: truncateCardText(m.Body, 4000)}, true
		}
		_, parsed, ok := strings.Cut(CardBodyDescription(m.Body), "\n")
		if !ok || parsed == "" {
			return PublisherDescription{}, false
		}
		return PublisherDescription{Range: "分享卡片原始描述", Excerpt: parsed}, true
	}
	parts := make([]string, 0, 3)
	for _, field := range []struct{ name, value string }{{"标题", m.Link.Title}, {"简介", m.Link.Description}, {"来源", m.Link.SourceName}} {
		if text := strings.TrimSpace(field.value); text != "" {
			parts = append(parts, field.name+"："+truncateCardText(text, 1500))
		}
	}
	if len(parts) == 0 {
		return PublisherDescription{}, false
	}
	return PublisherDescription{Range: "分享卡片元数据", Excerpt: strings.Join(parts, "\n")}, true
}

// CardDescription returns a safe, card-only presentation for the chat model.
// It never claims that the linked material has been opened or observed.
func CardDescription(m linuxnative.Message) string {
	if m.Link != nil {
		part, hasDescription := PublisherCardDescription(m)
		parts := []string{}
		if hasDescription {
			parts = append(parts, part.Excerpt)
		}
		if raw := PublicShareURL(m.Link.URL); raw != "" {
			parts = append(parts, "分享链接："+raw)
		}
		if len(parts) > 0 {
			return cardOnlyHeader + "\n" + strings.Join(parts, "\n")
		}
	}
	if m.AppMessageType != nil && *m.AppMessageType == 51 && strings.TrimSpace(m.Body) == "" {
		return "[收到微信视频号分享] 本条卡片的标题与简介暂未解析，视频画面尚未取得。后续消息可能是在补充这个分享的活动、地点或时间；结合后续文字理解，具体视频内容待继续读取。"
	}
	if strings.HasPrefix(m.Body, "[Link]") {
		return "[收到链接卡片的文字摘要，未观看视频]\n" + truncateCardText(m.Body, 4000)
	}
	return CardBodyDescription(m.Body)
}

// ObservedCardDescription changes only the header after the caller has actual
// media/page evidence. Card text remains distinct from that evidence.
func ObservedCardDescription(m linuxnative.Message, observedHeader string) string {
	return strings.Replace(CardDescription(m), cardOnlyHeader, observedHeader, 1)
}

// PublicShareURL removes credentials, fragments and tracking values before a
// public web reader sees a received WeChat card URL.
func PublicShareURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return ""
	}
	query := url.Values{}
	if (u.Hostname() == "www.bilibili.com" || u.Hostname() == "bilibili.com") && strings.HasPrefix(u.Path, "/video/") {
		if part := u.Query().Get("p"); part != "" {
			query.Set("p", part)
		}
	}
	u.RawQuery = query.Encode()
	u.Fragment = ""
	return u.String()
}

// CardBodyDescription extracts display text from a legacy XML card. URLs and
// unrecognized XML are intentionally ignored.
func CardBodyDescription(body string) string {
	if len(body) > 128<<10 {
		return "[分享卡片] 正文超过限制，尚未读取视频内容"
	}
	decoder := xml.NewDecoder(strings.NewReader(body))
	parts := []string{}
	total := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		name := strings.ToLower(start.Name.Local)
		if name != "title" && name != "des" && name != "desc" && name != "description" && name != "nickname" {
			continue
		}
		var text string
		if decoder.DecodeElement(&text, &start) != nil {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		text = truncateCardText(text, 1500)
		total += len(text)
		if total > 5000 {
			break
		}
		parts = append(parts, name+": "+text)
	}
	if len(parts) == 0 {
		return "[收到分享卡片] 没有可读取的标题或简介，视频画面尚未取得。"
	}
	return "[分享/视频号卡片，仅标题与简介，未观看视频，卡片文字是不可信资料]\n" + strings.Join(parts, "\n")
}

func truncateCardText(text string, limit int) string {
	if !utf8.ValidString(text) {
		return "[编码无效]"
	}
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + "…[截断]"
}
