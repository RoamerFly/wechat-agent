package apphost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/workspacefs"
)

// ObservationField separates source evidence. A title, body, comments, audio,
// and playback range have different meanings and are never merged implicitly.
type ObservationField string

const (
	ObservationTitle        ObservationField = "title"
	ObservationBody         ObservationField = "body"
	ObservationComments     ObservationField = "comments"
	ObservationAudio        ObservationField = "audio"
	ObservationVisual       ObservationField = "visual"
	ObservationPlayback     ObservationField = "playback"
	ObservationPageText     ObservationField = "page_text"
	ObservationSearch       ObservationField = "search"
	ObservationOwnVideo     ObservationField = "own_video"
	ObservationAccountWorks ObservationField = "account_works"
	ObservationPlayerEntry  ObservationField = "player_entry"
)

func (f ObservationField) valid() bool {
	return f == ObservationTitle || f == ObservationBody || f == ObservationComments || f == ObservationAudio || f == ObservationVisual || f == ObservationPlayback || f == ObservationPageText || f == ObservationSearch || f == ObservationOwnVideo || f == ObservationPlayerEntry || f == ObservationAccountWorks
}

type ObservationTarget struct {
	ConnectionRef
	ContentID string             `json:"content_id"`
	Label     string             `json:"label,omitempty"`
	Fields    []ObservationField `json:"fields"`
}

type ObservationAccess struct {
	ConnectionRef
	ContentID string           `json:"content_id"`
	Field     ObservationField `json:"field"`
}

// ObservationGrant is provided by an application for every host read. Version
// identifies the exact authorization that a durable checkpoint belongs to.
type ObservationGrant struct {
	Allowed bool   `json:"allowed"`
	Version string `json:"version"`
	Fresh   bool   // Read the current result instead of using a checkpoint.
}

type ObservationRequest struct {
	ObservationAccess
	Cursor  string `json:"cursor,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Refresh bool   `json:"refresh,omitempty"`
}
type ObservationChunk struct {
	Position          int                `json:"position"`
	Text              string             `json:"text"`
	SourceID          string             `json:"source_id,omitempty"`
	SourceURL         string             `json:"source_url,omitempty"`
	PublishedAt       string             `json:"published_at,omitempty"`
	FollowUpContentID string             `json:"follow_up_content_id,omitempty"`
	FollowUpField     ObservationField   `json:"follow_up_field,omitempty"`
	FollowUpFields    []ObservationField `json:"follow_up_fields,omitempty"`
}
type PlaybackInterval struct {
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
}

// PlaybackProof describes a bounded browser sample. FullyPlayed is derived
// by the host from the ended event and continuous browser-reported ranges.
type PlaybackProof struct {
	RequestedSeconds     int     `json:"requested_seconds"`
	MediaDurationSeconds float64 `json:"media_duration_seconds,omitempty"`
	Ready                bool    `json:"ready"`
	Ended                bool    `json:"ended"`
	Paused               bool    `json:"paused"`
	FullyPlayed          bool    `json:"fully_played"`
}

// AudioProof describes a decoded window and the recognition outcome. Decoded
// seconds measure PCM handed to the recognizer, independent of the window size.
type AudioProof struct {
	State                string  `json:"state"`
	MediaSHA256          string  `json:"media_sha256"`
	RequestedSeconds     int     `json:"requested_seconds"`
	StartSeconds         float64 `json:"start_seconds"`
	WindowEndSeconds     float64 `json:"window_end_seconds"`
	DecodedSeconds       float64 `json:"decoded_seconds"`
	MediaDurationSeconds float64 `json:"media_duration_seconds"`
}

// VisualFrameProof binds an analysis batch to actual decoded video frames.
// Frame indexes are logical extraction indexes. Timestamps are decoded PTS and
// may repeat when the media container reports identical presentation times.
type VisualFrameProof struct {
	MediaSHA256     string `json:"media_sha256"`
	RequestedFrames int    `json:"requested_frames"`
	StartFrame      int    `json:"start_frame"`
	NextFrame       int    `json:"next_frame"`
	// FrameCount is the total number of logical decodable frames in the source,
	// not merely the number returned by this batch. FrameStep is the stable
	// extraction stride used by every page of the same observation.
	FrameCount           int       `json:"frame_count"`
	FrameStep            int       `json:"frame_step"`
	MediaDurationSeconds float64   `json:"media_duration_seconds"`
	FrameIndexes         []int     `json:"frame_indexes"`
	TimestampSeconds     []float64 `json:"timestamp_seconds"`
}

// OwnVideoProof records the official status of an exact account-owned video
// at a specific observation time. It carries no posting or delivery receipt.
type OwnVideoProof struct {
	VideoID    string `json:"video_id"`
	StatusCode int    `json:"status_code"`
	State      string `json:"state"`
	ObservedAt string `json:"observed_at"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// ObservationPage has evidence for only the requested field. A provider must
// return a new opaque cursor until that field is complete.
type ObservationPage struct {
	Chunks        []ObservationChunk `json:"chunks,omitempty"`
	Playback      []PlaybackInterval `json:"playback,omitempty"`
	PlaybackProof *PlaybackProof     `json:"playback_proof,omitempty"`
	AudioProof    *AudioProof        `json:"audio_proof,omitempty"`
	VisualProof   *VisualFrameProof  `json:"visual_proof,omitempty"`
	OwnVideoProof *OwnVideoProof     `json:"own_video_proof,omitempty"`
	Coverage      string             `json:"coverage"`
	Complete      bool               `json:"complete"`
	NextCursor    string             `json:"-"`
}

// ObservationProvider is an optional read-only capability for a registered
// application. The station itself never invents discoverability or grants.
type ObservationProvider interface {
	ObservationTargets(context.Context, string) ([]ObservationTarget, error)
	ObservationGrant(context.Context, ObservationAccess) (ObservationGrant, error)
	ReadObservation(context.Context, ObservationRequest) (ObservationPage, error)
}

type ObservationInput struct {
	AppID        string           `json:"app_id"`
	ConnectionID string           `json:"connection_id"`
	ContentID    string           `json:"content_id"`
	Field        ObservationField `json:"field"`
	Limit        int              `json:"limit,omitempty"`
	Refresh      bool             `json:"refresh,omitempty"`
}
type ObservationResult struct {
	ConnectionRef
	ContentID     string             `json:"content_id"`
	Field         ObservationField   `json:"field"`
	GrantVersion  string             `json:"-"`
	Chunks        []ObservationChunk `json:"chunks,omitempty"`
	Playback      []PlaybackInterval `json:"playback,omitempty"`
	PlaybackProof *PlaybackProof     `json:"playback_proof,omitempty"`
	AudioProof    *AudioProof        `json:"audio_proof,omitempty"`
	VisualProof   *VisualFrameProof  `json:"visual_proof,omitempty"`
	OwnVideoProof *OwnVideoProof     `json:"own_video_proof,omitempty"`
	Coverage      string             `json:"coverage"`
	Complete      bool               `json:"complete"`
	Cached        bool               `json:"cached"`
	Resumed       bool               `json:"resumed"`
}

var (
	ErrObservationInvalid                  = errors.New("应用观察请求无效")
	ErrObservationNotFound                 = errors.New("未找到已接入的应用或账号")
	ErrObservationForbidden                = errors.New("当前应用账号未授权读取此内容")
	ErrObservationContentUnavailable       = errors.New("平台暂时未提供这条内容，请核对编号或公开状态")
	ErrObservationThrottled                = errors.New("平台限制了本次访问，请稍后重试")
	ErrObservationBrowserUnavailable       = errors.New("独立浏览器暂不可用，请检查本机 Microsoft Edge 与应用授权")
	ErrObservationWebSessionBusy           = errors.New("网页登录资料正在使用，请完成登录或稍后重试读取")
	ErrObservationWebSessionRequired       = errors.New("网页要求登录或验证，请由主人打开抖音网页登录窗口处理")
	ErrObservationWebSessionInvalid        = errors.New("网页登录资料登记不可用，请在抖音应用详情核对登录资料")
	ErrObservationWebStorageUnavailable    = errors.New("网页读取进度无法读取或保存，请检查本机存储权限和磁盘空间")
	ErrObservationWebReadTimeout           = errors.New("网页读取或播放观察超时，可保留当前授权稍后重试")
	ErrObservationWebSnapshotChanged       = errors.New("网页读取快照已变化或不可用，请使用 refresh=true 从头读取")
	ErrObservationPageTextSize             = errors.New("网页正文超出本机单页传输范围，未返回截断内容；可读取更具体的内容入口")
	ErrObservationRemotePlayerUnavailable  = errors.New("抖音官方播放器暂未提供可采样的媒体，可稍后重试")
	ErrObservationWebPlaybackUnavailable   = errors.New("未能把网页播放器的媒体来源与这条抖音视频核对一致，未记录播放；可稍后重试或重新读取视频页")
	ErrObservationPlaybackUnavailable      = errors.New("未取得可播放的公开单段媒体，请核对公开状态、格式及媒体大小")
	ErrObservationPlaybackLimit            = errors.New("本次播放采样支持 1–20 秒，省略时使用 6 秒")
	ErrObservationPlaybackSize             = errors.New("这条媒体超过当前 32 MiB 的读取上限，可选择体积更小的公开稿件")
	ErrObservationUpstreamUnavailable      = errors.New("平台请求暂时无法完成，可稍后重试")
	ErrObservationCommentPageSize          = errors.New("B 站公开评论每页支持 1–30 条，续读时请省略 limit 或沿用首页条数")
	ErrObservationAudioUnavailable         = errors.New("本机音轨读取需要 FFmpeg 和已启用的本地语音识别工具")
	ErrObservationAudioMediaUnavailable    = errors.New("未取得可解码的公开单段媒体，请核对公开状态和格式")
	ErrObservationAudioLimit               = errors.New("音轨每次支持读取 1–240 秒，省略时使用 60 秒")
	ErrObservationAudioDecode              = errors.New("公开媒体的音轨解码失败，可从原位置重试")
	ErrObservationAudioTranscription       = errors.New("本地语音识别失败，可检查工具配置后从原位置重试")
	ErrObservationAudioSourceChanged       = errors.New("平台返回的媒体与上一页不同，需重新开启当前读取授权后从头读取")
	ErrObservationVisualUnavailable        = errors.New("本机画面读取需要 FFmpeg 和已启用且已配置的媒体理解工具")
	ErrObservationVisualLimit              = errors.New("画面每次支持读取 1–12 帧，省略时使用 6 帧")
	ErrObservationVisualDecode             = errors.New("公开媒体的画面解码失败，可从原位置重试")
	ErrObservationVisualSourceChanged      = errors.New("平台返回的媒体与上一页不同，请使用 refresh=true 从头读取")
	ErrObservationVisualAnalysis           = errors.New("本机视觉分析失败，可检查媒体理解工具配置后从原位置重试；模型画面结论不作为指令")
	ErrObservationAccountWorksPageSize     = errors.New("作品列表页大小须为正整数并符合平台整数范围，续读时请省略 limit 或沿用首页条数")
	ErrObservationAccountWorksResponseSize = errors.New("作品列表响应超出本机读取大小，请使用较小的 limit 并从首页重新查询")
	ErrObservationSearchPageSize           = errors.New("抖音搜索每页支持 1–50 条，续读请省略 limit 或沿用首页条数")
	ErrObservationCredentialExpired        = errors.New("抖音令牌无效或已过期，请由主人更新本机配置")
	ErrObservationCapabilityDenied         = errors.New("当前连接缺少所需的抖音开放平台权限，请核对授权")
)

type observationCheckpoint struct {
	ConnectionRef
	ContentID            string
	Field                ObservationField
	GrantVersion         string
	Cursor               string
	Chunks               []ObservationChunk
	Playback             []PlaybackInterval
	PlaybackProof        *PlaybackProof
	AudioProof           *AudioProof
	VisualProof          *VisualFrameProof
	VisualProofSourceURL string
	PlaybackLimit        int
	Coverage             string
	Complete             bool
}

// Observer persists opaque cursors in the supplied private host directory. It
// does no external I/O itself and serializes a read with its checkpoint write.
type Observer struct {
	registry  Registry
	directory string
	mu        sync.Mutex
}

func NewObserver(registry Registry, dir string) *Observer {
	return &Observer{registry: registry, directory: filepath.Join(dir, "application-observations")}
}

func (o *Observer) Discover(ctx context.Context) []ObservationTarget {
	return o.discover(ctx, false)
}

func (o *Observer) HasDiscoverable(ctx context.Context) bool {
	return len(o.discover(ctx, true)) > 0
}

func (o *Observer) discover(ctx context.Context, firstOnly bool) []ObservationTarget {
	if o == nil || ctx.Err() != nil {
		return nil
	}
	result := make([]ObservationTarget, 0)
	for _, app := range o.registry.Applications() {
		entry, ok := o.registry.Lookup(app.ID)
		if !ok {
			continue
		}
		provider, ok := entry.Provider.(ObservationProvider)
		if !ok {
			continue
		}
		connections, err := entry.Provider.Connections(ctx)
		if err != nil {
			continue
		}
		for _, connection := range connections {
			targets, err := provider.ObservationTargets(ctx, connection.ID)
			if err != nil {
				continue
			}
			for _, target := range targets {
				if target.ContentID == "" {
					continue
				}
				target.ConnectionRef = ConnectionRef{AppID: app.ID, InstallationID: app.InstallationID, ConnectionID: connection.ID, AccountID: connection.AccountID}
				candidates := target.Fields
				if len(candidates) == 0 {
					candidates = []ObservationField{ObservationTitle, ObservationBody, ObservationComments, ObservationAudio, ObservationPlayback}
				}
				target.Fields = nil
				for _, field := range candidates {
					if !field.valid() || containsObservationField(target.Fields, field) {
						continue
					}
					grant, grantErr := provider.ObservationGrant(ctx, ObservationAccess{ConnectionRef: target.ConnectionRef, ContentID: target.ContentID, Field: field})
					if grantErr == nil && grant.Allowed && strings.TrimSpace(grant.Version) != "" {
						if firstOnly {
							return []ObservationTarget{target}
						}
						target.Fields = append(target.Fields, field)
					}
				}
				if len(target.Fields) == 0 {
					continue
				}
				if !containsObservationField(target.Fields, ObservationTitle) && !containsObservationField(target.Fields, ObservationSearch) && !containsObservationField(target.Fields, ObservationAccountWorks) {
					target.Label = ""
				}
				result = append(result, target)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AppID != result[j].AppID {
			return result[i].AppID < result[j].AppID
		}
		if result[i].ConnectionID != result[j].ConnectionID {
			return result[i].ConnectionID < result[j].ConnectionID
		}
		return result[i].ContentID < result[j].ContentID
	})
	return result
}

func containsObservationField(fields []ObservationField, wanted ObservationField) bool {
	for _, field := range fields {
		if field == wanted {
			return true
		}
	}
	return false
}

func (o *Observer) Read(ctx context.Context, in ObservationInput) (ObservationResult, error) {
	if err := ctx.Err(); err != nil {
		return ObservationResult{}, err
	}
	if o == nil || !in.Field.valid() || strings.TrimSpace(in.AppID) == "" || strings.TrimSpace(in.ConnectionID) == "" || strings.TrimSpace(in.ContentID) == "" || in.Limit < 0 || in.Refresh && in.Field != ObservationSearch && in.Field != ObservationAccountWorks && in.Field != ObservationPageText && in.Field != ObservationPlayback && in.Field != ObservationAudio && in.Field != ObservationVisual && in.Field != ObservationComments {
		return ObservationResult{}, ErrObservationInvalid
	}
	// Check an explicit frame batch size before consulting a checkpoint. A
	// cached result must not turn an invalid request into a successful read.
	if in.Field == ObservationVisual && in.Limit != 0 && (in.Limit < 1 || in.Limit > 12) {
		return ObservationResult{}, ErrObservationVisualLimit
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	provider, ref, grant, err := o.authorize(ctx, in)
	if err != nil {
		return ObservationResult{}, err
	}
	playbackLimit := 0
	if in.Field == ObservationPlayback {
		playbackLimit = in.Limit
	}
	visualLimit := 0
	if in.Field == ObservationVisual {
		visualLimit = in.Limit
		if visualLimit == 0 {
			visualLimit = 6
		}
	}
	key := observationKey(ref, in.ContentID, in.Field, grant.Version, playbackLimit)
	var checkpoint observationCheckpoint
	var existed bool
	fresh := grant.Fresh || in.Field == ObservationOwnVideo || in.Field == ObservationPlayerEntry
	if !in.Refresh && !fresh {
		checkpoint, existed, err = o.load(key)
	}
	if err != nil {
		return ObservationResult{}, errors.New("应用观察进度不可用")
	}
	if existed && (!checkpoint.matches(ref, in.ContentID, in.Field, grant.Version) || checkpoint.PlaybackLimit != playbackLimit) {
		return ObservationResult{}, errors.New("应用观察进度与当前授权不一致")
	}
	if checkpoint.Complete {
		if err := o.Check(ctx, checkpoint.result(false, false)); err != nil {
			return ObservationResult{}, err
		}
		return checkpoint.result(true, false), nil
	}
	page, err := provider.ReadObservation(ctx, ObservationRequest{ObservationAccess: ObservationAccess{ConnectionRef: ref, ContentID: in.ContentID, Field: in.Field}, Cursor: checkpoint.Cursor, Limit: in.Limit, Refresh: in.Refresh})
	if err != nil {
		if ctx.Err() != nil {
			return ObservationResult{}, ctx.Err()
		}
		for _, known := range []error{
			ErrObservationContentUnavailable, ErrObservationThrottled, ErrObservationForbidden, ErrObservationInvalid,
			ErrObservationBrowserUnavailable, ErrObservationPlaybackUnavailable, ErrObservationRemotePlayerUnavailable, ErrObservationWebPlaybackUnavailable, ErrObservationPlaybackLimit,
			ErrObservationWebSessionBusy, ErrObservationWebSessionRequired, ErrObservationWebSnapshotChanged, ErrObservationPageTextSize,
			ErrObservationWebSessionInvalid, ErrObservationWebStorageUnavailable,
			ErrObservationWebReadTimeout,
			ErrObservationPlaybackSize, ErrObservationUpstreamUnavailable, ErrObservationCommentPageSize,
			ErrObservationAudioUnavailable, ErrObservationAudioMediaUnavailable, ErrObservationAudioLimit,
			ErrObservationAudioDecode, ErrObservationAudioTranscription, ErrObservationAudioSourceChanged,
			ErrObservationVisualUnavailable, ErrObservationVisualLimit, ErrObservationVisualDecode,
			ErrObservationVisualSourceChanged, ErrObservationVisualAnalysis,
			ErrObservationSearchPageSize, ErrObservationAccountWorksPageSize, ErrObservationAccountWorksResponseSize, ErrObservationCredentialExpired, ErrObservationCapabilityDenied,
		} {
			if errors.Is(err, known) {
				return ObservationResult{}, known
			}
		}
		return ObservationResult{}, errors.New("应用内容暂时无法读取")
	}
	if err := validateObservationPage(in.Field, page); err != nil {
		return ObservationResult{}, err
	}
	if in.Limit > 0 && page.AudioProof != nil && page.AudioProof.RequestedSeconds != in.Limit {
		return ObservationResult{}, errors.New("音轨证据与请求的读取时长不一致")
	}
	if page.AudioProof != nil && !existed && page.AudioProof.StartSeconds != 0 {
		return ObservationResult{}, errors.New("音轨的首段读取未从开头开始")
	}
	if page.AudioProof != nil && checkpoint.AudioProof != nil &&
		(page.AudioProof.MediaSHA256 != checkpoint.AudioProof.MediaSHA256 ||
			page.AudioProof.MediaDurationSeconds != checkpoint.AudioProof.MediaDurationSeconds ||
			page.AudioProof.StartSeconds != checkpoint.AudioProof.WindowEndSeconds) {
		return ObservationResult{}, ErrObservationAudioSourceChanged
	}
	if page.VisualProof != nil && page.VisualProof.RequestedFrames != visualLimit {
		return ObservationResult{}, errors.New("画面证据与请求的读取帧数不一致")
	}
	if page.VisualProof != nil && !existed && page.VisualProof.StartFrame != 0 {
		return ObservationResult{}, errors.New("画面的首段读取未从开头开始")
	}
	if page.VisualProof != nil && checkpoint.VisualProof != nil &&
		(page.VisualProof.MediaSHA256 != checkpoint.VisualProof.MediaSHA256 ||
			page.VisualProof.MediaDurationSeconds != checkpoint.VisualProof.MediaDurationSeconds ||
			page.VisualProof.FrameCount != checkpoint.VisualProof.FrameCount ||
			page.VisualProof.FrameStep != checkpoint.VisualProof.FrameStep ||
			page.VisualProof.StartFrame != checkpoint.VisualProof.NextFrame ||
			page.Chunks[0].SourceURL != checkpoint.VisualProofSourceURL ||
			page.VisualProof.TimestampSeconds[0] < checkpoint.VisualProof.TimestampSeconds[len(checkpoint.VisualProof.TimestampSeconds)-1]) {
		return ObservationResult{}, ErrObservationVisualSourceChanged
	}
	if playbackLimit > 0 && page.PlaybackProof != nil && page.PlaybackProof.RequestedSeconds != playbackLimit {
		return ObservationResult{}, errors.New("播放证据与请求的采样时间不一致")
	}
	if strings.TrimSpace(page.Coverage) == "" || (!page.Complete && (page.NextCursor == "" || page.NextCursor == checkpoint.Cursor)) {
		return ObservationResult{}, errors.New("应用内容未返回可继续的观察进度")
	}
	if err := ctx.Err(); err != nil {
		return ObservationResult{}, err
	}
	if err := o.Check(ctx, ObservationResult{ConnectionRef: ref, ContentID: in.ContentID, Field: in.Field, GrantVersion: grant.Version}); err != nil {
		return ObservationResult{}, err
	}
	// Review state and player availability can change. Always check upstream.
	if in.Field == ObservationOwnVideo {
		if page.OwnVideoProof.VideoID != in.ContentID {
			return ObservationResult{}, errors.New("作品状态与请求编号不一致")
		}
		return ObservationResult{ConnectionRef: ref, ContentID: in.ContentID, Field: in.Field, GrantVersion: grant.Version,
			Chunks: page.Chunks, OwnVideoProof: page.OwnVideoProof, Coverage: page.Coverage, Complete: true}, nil
	}
	checkpoint = observationCheckpoint{
		ConnectionRef: ref, ContentID: in.ContentID, Field: in.Field, GrantVersion: grant.Version,
		Cursor: page.NextCursor, Chunks: append([]ObservationChunk(nil), page.Chunks...), Playback: append([]PlaybackInterval(nil), page.Playback...),
		PlaybackProof: verifiedPlaybackProof(page), AudioProof: page.AudioProof, VisualProof: page.VisualProof, VisualProofSourceURL: visualSourceURL(page), PlaybackLimit: playbackLimit, Coverage: page.Coverage, Complete: page.Complete,
	}
	if checkpoint.Complete {
		checkpoint.Cursor = ""
	}
	// A fresh observation and a stalled sample both need another upstream read.
	if fresh || checkpoint.PlaybackProof != nil && checkpoint.Complete && len(checkpoint.Playback) == 0 {
		return checkpoint.result(false, false), nil
	}
	stored := checkpoint
	if in.Field == ObservationAccountWorks {
		// Account lists may describe non-public works. Persist only progress.
		stored.Chunks = nil
		if stored.Complete {
			stored.Coverage += "。可用 refresh=true 重新读取。"
		}
	}
	if err := o.save(key, stored); err != nil {
		return ObservationResult{}, errors.New("应用观察进度无法保存")
	}
	return checkpoint.result(false, existed), nil
}

// Check repeats the exact account- and grant-version authorization for
// evidence already read.  A caller uses it after model work and before any
// action based on the observation, without re-reading a provider page.
func (o *Observer) Check(ctx context.Context, result ObservationResult) error {
	if o == nil || !result.Field.valid() || result.AppID == "" || result.ConnectionID == "" || result.ContentID == "" || result.GrantVersion == "" {
		return ErrObservationInvalid
	}
	_, ref, grant, err := o.authorize(ctx, ObservationInput{AppID: result.AppID, ConnectionID: result.ConnectionID, ContentID: result.ContentID, Field: result.Field})
	if err != nil {
		return err
	}
	if ref != result.ConnectionRef || grant.Version != result.GrantVersion {
		return ErrObservationForbidden
	}
	return nil
}

func validateObservationPage(field ObservationField, page ObservationPage) error {
	if field == ObservationVisual && page.VisualProof == nil {
		return errors.New("应用未返回画面读取证据")
	}
	if field == ObservationOwnVideo || page.OwnVideoProof != nil {
		if field != ObservationOwnVideo || !validOwnVideoProof(page) {
			return errors.New("应用返回了不一致的本人作品状态")
		}
	}
	if page.AudioProof != nil {
		if field != ObservationAudio || !validAudioProof(*page.AudioProof, page.Complete, len(page.Chunks)) {
			return errors.New("应用内容返回了不一致的音轨证据")
		}
	}
	if page.VisualProof != nil {
		if field != ObservationVisual || !validVisualProof(*page.VisualProof, page) {
			return errors.New("应用内容返回了不一致的画面证据")
		}
	}
	if field == ObservationPlayback {
		if len(page.Chunks) != 0 {
			return errors.New("应用内容返回了不匹配的观察字段")
		}
		for _, progress := range page.Playback {
			if math.IsNaN(progress.StartSeconds) || math.IsNaN(progress.EndSeconds) || math.IsInf(progress.StartSeconds, 0) || math.IsInf(progress.EndSeconds, 0) || progress.StartSeconds < 0 || progress.EndSeconds <= progress.StartSeconds {
				return errors.New("应用内容返回了无效的播放进度")
			}
		}
		if proof := page.PlaybackProof; proof != nil {
			if proof.RequestedSeconds <= 0 || math.IsNaN(proof.MediaDurationSeconds) || math.IsInf(proof.MediaDurationSeconds, 0) ||
				proof.MediaDurationSeconds < 0 || !proof.Ready && (proof.Ended || len(page.Playback) > 0) {
				return errors.New("应用返回了不一致的播放状态")
			}
			var previousEnd float64
			for _, progress := range page.Playback {
				if progress.StartSeconds < previousEnd || proof.MediaDurationSeconds > 0 && progress.EndSeconds > proof.MediaDurationSeconds+0.05 {
					return errors.New("已播放区间与媒体时间轴不一致")
				}
				previousEnd = progress.EndSeconds
			}
		}
		return nil
	}
	if len(page.Playback) != 0 || page.PlaybackProof != nil {
		return errors.New("应用内容返回了不匹配的观察字段")
	}
	if field == ObservationVisual {
		return nil
	}
	if field == ObservationPlayerEntry && (!page.Complete || page.NextCursor != "" || len(page.Chunks) != 1 || page.Chunks[0].SourceID == "" || page.Chunks[0].SourceURL == "") {
		return errors.New("应用未返回可核对的播放器入口")
	}
	position := -1
	for _, chunk := range page.Chunks {
		if chunk.FollowUpContentID != "" || chunk.FollowUpField != "" || len(chunk.FollowUpFields) > 0 {
			if !validObservationFollowUp(field, chunk.FollowUpField) || chunk.SourceID == "" || strings.TrimSpace(chunk.FollowUpContentID) == "" || len(chunk.FollowUpContentID) > 256 || len(chunk.FollowUpFields) > 5 {
				return errors.New("应用内容返回了无效的后续读取编号")
			}
			seen := map[ObservationField]bool{}
			for _, next := range chunk.FollowUpFields {
				if seen[next] || !validObservationFollowUp(field, next) {
					return errors.New("应用内容返回了无效的后续读取字段")
				}
				seen[next] = true
			}
			if len(seen) > 0 && !seen[chunk.FollowUpField] {
				return errors.New("后续默认读取字段与候选字段不一致")
			}
		}
		if strings.TrimSpace(chunk.Text) == "" || chunk.Position <= position {
			return errors.New("应用内容返回了无效的观察片段")
		}
		position = chunk.Position
	}
	return nil
}

func validObservationFollowUp(field, next ObservationField) bool {
	return field == ObservationSearch && next == ObservationPlayerEntry || field == ObservationPlayerEntry && next == ObservationPlayback || field == ObservationPageText && (next == ObservationPageText || next == ObservationPlayback || next == ObservationAudio || next == ObservationVisual || next == ObservationComments) || field == ObservationVisual && next == ObservationVisual
}

func validVisualProof(proof VisualFrameProof, page ObservationPage) bool {
	decodedHash, err := hex.DecodeString(proof.MediaSHA256)
	if err != nil || len(decodedHash) != sha256.Size || proof.RequestedFrames < 1 || proof.RequestedFrames > 12 ||
		proof.FrameCount < 1 || proof.FrameStep < 1 || proof.StartFrame < 0 || proof.StartFrame >= proof.FrameCount ||
		len(proof.FrameIndexes) != min(proof.RequestedFrames, (proof.FrameCount-proof.StartFrame-1)/proof.FrameStep+1) ||
		len(proof.FrameIndexes) != len(proof.TimestampSeconds) || len(page.Chunks) != 1 ||
		strings.TrimSpace(page.Chunks[0].Text) == "" || page.Chunks[0].SourceID != proof.MediaSHA256 || strings.TrimSpace(page.Chunks[0].SourceURL) == "" ||
		page.Chunks[0].Position != proof.StartFrame || page.Chunks[0].FollowUpContentID != "" || page.Chunks[0].FollowUpField != "" || len(page.Chunks[0].FollowUpFields) != 0 ||
		math.IsNaN(proof.MediaDurationSeconds) || math.IsInf(proof.MediaDurationSeconds, 0) || proof.MediaDurationSeconds <= 0 {
		return false
	}
	parsedSource, err := url.Parse(page.Chunks[0].SourceURL)
	if err != nil || parsedSource.Scheme != "https" || parsedSource.Host == "" || parsedSource.User != nil {
		return false
	}
	if proof.FrameIndexes[0] != proof.StartFrame {
		return false
	}
	previousIndex := -1
	previousTimestamp := -1.0
	for i, index := range proof.FrameIndexes {
		if index < 0 || index >= proof.FrameCount || i > 0 && index != previousIndex+proof.FrameStep {
			return false
		}
		timestamp := proof.TimestampSeconds[i]
		if math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp < 0 || timestamp > proof.MediaDurationSeconds+0.05 || timestamp < previousTimestamp {
			return false
		}
		previousIndex, previousTimestamp = index, timestamp
	}
	last := proof.FrameIndexes[len(proof.FrameIndexes)-1]
	expectedNext := last + min(proof.FrameStep, proof.FrameCount-last)
	if proof.NextFrame != expectedNext || page.Complete != (proof.NextFrame >= proof.FrameCount) || page.Complete && page.NextCursor != "" || !page.Complete && page.NextCursor == "" {
		return false
	}
	return true
}

func visualSourceURL(page ObservationPage) string {
	if page.VisualProof == nil || len(page.Chunks) != 1 {
		return ""
	}
	return page.Chunks[0].SourceURL
}

func validOwnVideoProof(page ObservationPage) bool {
	proof := page.OwnVideoProof
	if proof == nil || strings.TrimSpace(proof.VideoID) == "" || !page.Complete || page.NextCursor != "" {
		return false
	}
	observed, err := time.Parse(time.RFC3339, proof.ObservedAt)
	if err != nil || observed.IsZero() {
		return false
	}
	switch proof.State {
	case "public":
		if proof.CreatedAt != "" {
			created, err := time.Parse(time.RFC3339, proof.CreatedAt)
			if err != nil || created.IsZero() {
				return false
			}
		}
		return len(page.Chunks) <= 1 && (len(page.Chunks) == 0 ||
			(page.Chunks[0].SourceID == proof.VideoID && page.Chunks[0].SourceURL == "" && page.Chunks[0].PublishedAt == ""))
	case "not_public", "reviewing", "unknown":
		return len(page.Chunks) == 0 && proof.CreatedAt == ""
	default:
		return false
	}
}

func validAudioProof(proof AudioProof, complete bool, chunks int) bool {
	for _, value := range []float64{proof.StartSeconds, proof.WindowEndSeconds, proof.DecodedSeconds, proof.MediaDurationSeconds} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return false
		}
	}
	decodedHash, err := hex.DecodeString(proof.MediaSHA256)
	if err != nil || len(decodedHash) != sha256.Size || proof.RequestedSeconds < 1 ||
		proof.WindowEndSeconds <= proof.StartSeconds ||
		proof.WindowEndSeconds != min(proof.MediaDurationSeconds, proof.StartSeconds+float64(proof.RequestedSeconds)) ||
		proof.DecodedSeconds > proof.WindowEndSeconds-proof.StartSeconds+0.001 {
		return false
	}
	atEnd := proof.WindowEndSeconds == proof.MediaDurationSeconds
	switch proof.State {
	case "transcribed":
		return chunks > 0 && proof.DecodedSeconds > 0 && complete == atEnd
	case "no_recognizable_speech":
		return chunks == 0 && proof.DecodedSeconds > 0 && complete == atEnd
	case "no_audio_in_range":
		return chunks == 0 && proof.DecodedSeconds == 0 && complete == atEnd
	case "no_audio_track":
		return chunks == 0 && proof.DecodedSeconds == 0 && complete
	default:
		return false
	}
}

func verifiedPlaybackProof(page ObservationPage) *PlaybackProof {
	if page.PlaybackProof == nil {
		return nil
	}
	proof := *page.PlaybackProof
	proof.FullyPlayed = proof.Ready && proof.Ended && proof.Paused && proof.MediaDurationSeconds > 0 &&
		len(page.Playback) == 1 && page.Playback[0].StartSeconds == 0 && page.Playback[0].EndSeconds >= proof.MediaDurationSeconds
	return &proof
}

func (o *Observer) authorize(ctx context.Context, in ObservationInput) (ObservationProvider, ConnectionRef, ObservationGrant, error) {
	if err := ctx.Err(); err != nil {
		return nil, ConnectionRef{}, ObservationGrant{}, err
	}
	entry, ok := o.registry.Lookup(in.AppID)
	if !ok {
		return nil, ConnectionRef{}, ObservationGrant{}, ErrObservationNotFound
	}
	provider, ok := entry.Provider.(ObservationProvider)
	if !ok {
		return nil, ConnectionRef{}, ObservationGrant{}, ErrObservationNotFound
	}
	connections, err := entry.Provider.Connections(ctx)
	if err != nil {
		return nil, ConnectionRef{}, ObservationGrant{}, errors.New("应用连接暂时不可用")
	}
	var connection Connection
	found := false
	for _, candidate := range connections {
		if candidate.ID == in.ConnectionID {
			connection = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, ConnectionRef{}, ObservationGrant{}, ErrObservationNotFound
	}
	ref := ConnectionRef{AppID: entry.ID, InstallationID: entry.InstallationID, ConnectionID: connection.ID, AccountID: connection.AccountID}
	grant, err := provider.ObservationGrant(ctx, ObservationAccess{ConnectionRef: ref, ContentID: in.ContentID, Field: in.Field})
	for _, known := range []error{ErrObservationAudioUnavailable, ErrObservationCredentialExpired, ErrObservationCapabilityDenied} {
		if errors.Is(err, known) {
			return nil, ConnectionRef{}, ObservationGrant{}, known
		}
	}
	if err != nil || !grant.Allowed || strings.TrimSpace(grant.Version) == "" {
		return nil, ConnectionRef{}, ObservationGrant{}, ErrObservationForbidden
	}
	return provider, ref, grant, nil
}

func (o *Observer) load(key string) (observationCheckpoint, bool, error) {
	root, err := workspacefs.EnsurePath(o.directory)
	if err != nil {
		return observationCheckpoint{}, false, err
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, key+".json")
	if errors.Is(err, os.ErrNotExist) {
		return observationCheckpoint{}, false, nil
	}
	if err != nil {
		return observationCheckpoint{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	var checkpoint observationCheckpoint
	if err != nil || json.Unmarshal(data, &checkpoint) != nil {
		return observationCheckpoint{}, false, errors.New("invalid checkpoint")
	}
	return checkpoint, true, nil
}
func (o *Observer) save(key string, checkpoint observationCheckpoint) error {
	root, err := workspacefs.EnsurePath(o.directory)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	var entropy [16]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return err
	}
	temporary := "." + key + "." + hex.EncodeToString(entropy[:]) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temporary) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = root.Rename(temporary, key+".json"); err != nil {
		return err
	}
	return nil
}
func observationKey(ref ConnectionRef, content string, field ObservationField, version string, playbackLimit int) string {
	encoded, _ := json.Marshal(struct {
		App, Installation, Connection, Account, Content string
		Field                                           ObservationField
		Version                                         string
		PlaybackLimit                                   int `json:"playback_limit,omitempty"`
	}{ref.AppID, ref.InstallationID, ref.ConnectionID, ref.AccountID, content, field, version, playbackLimit})
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}
func (c observationCheckpoint) result(cached, resumed bool) ObservationResult {
	return ObservationResult{ConnectionRef: c.ConnectionRef, ContentID: c.ContentID, Field: c.Field, GrantVersion: c.GrantVersion, Chunks: append([]ObservationChunk(nil), c.Chunks...), Playback: append([]PlaybackInterval(nil), c.Playback...), PlaybackProof: c.PlaybackProof, AudioProof: c.AudioProof, VisualProof: c.VisualProof, Coverage: c.Coverage, Complete: c.Complete, Cached: cached, Resumed: resumed}
}

func (c observationCheckpoint) matches(ref ConnectionRef, contentID string, field ObservationField, version string) bool {
	return c.ConnectionRef == ref && c.ContentID == contentID && c.Field == field && c.GrantVersion == version
}
