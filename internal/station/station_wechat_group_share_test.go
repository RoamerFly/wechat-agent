package station

import (
	"strings"
	"testing"

	"wechat-bot/internal/companion"
)

func TestStationGroupShareEvidenceTextExcludesPublisherMetadata(t *testing.T) {
	rangeText, excerpt := stationGroupShareEvidenceText([]companion.ObservationPart{
		{Kind: companion.ObservationPublisherDescription, Range: "card", Excerpt: "发布者标题"},
		{Kind: companion.ObservationAudioTranscript, Range: "视频音轨 0.000–5.000 秒", Excerpt: "实际音轨文字"},
		{Kind: companion.ObservationVisualFrames, Range: "视频采样帧：1秒、3秒", Excerpt: "实际画面内容"},
	})
	if strings.Contains(rangeText, "card") || strings.Contains(excerpt, "发布者标题") {
		t.Fatalf("publisher metadata entered automatic evidence: range=%q excerpt=%q", rangeText, excerpt)
	}
	if !strings.Contains(rangeText, "视频音轨") || !strings.Contains(rangeText, "视频采样帧") ||
		!strings.Contains(excerpt, "音轨转写：实际音轨文字") || !strings.Contains(excerpt, "画面观察：实际画面内容") {
		t.Fatalf("firsthand evidence not retained: range=%q excerpt=%q", rangeText, excerpt)
	}
}

func TestStationGroupShareEvidenceTextRejectsNonFirsthandOnly(t *testing.T) {
	rangeText, excerpt := stationGroupShareEvidenceText([]companion.ObservationPart{{
		Kind: companion.ObservationPublisherDescription, Range: "card", Excerpt: "仅卡片文字",
	}})
	if rangeText != "" || excerpt != "" {
		t.Fatalf("metadata-only card must not become evidence: range=%q excerpt=%q", rangeText, excerpt)
	}
}
