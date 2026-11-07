package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/mediaspec"
)

const (
	defaultAudioSeconds = 60
	maxAudioSeconds     = 240 // Mono PCM16 at 16 kHz fits the decoder's 8 MiB limit.
)

type audioCursor struct {
	MediaSHA256 string
	NextSeconds float64
	Position    int
}

// SetAudioTranscriber binds the station's local-only media tool at startup.
// The version changes when the tool or its authorization/configuration changes.
func (p *PublicApp) SetAudioTranscriber(version func(context.Context) (string, error), transcribe func(context.Context, []byte, func() error) (string, error)) {
	p.audioVersion = version
	p.transcribeAudio = transcribe
}

func (p *PublicApp) AudioAvailable(ctx context.Context) bool {
	_, err := p.localAudioVersion(ctx)
	return err == nil
}

func (p *PublicApp) localAudioVersion(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.audioVersion == nil || p.transcribeAudio == nil || !mediadecode.Available() {
		return "", apphost.ErrObservationAudioUnavailable
	}
	version, err := p.audioVersion(ctx)
	if err != nil || strings.TrimSpace(version) == "" {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", apphost.ErrObservationAudioUnavailable
	}
	return version, nil
}

func (p *PublicApp) readAudio(ctx context.Context, request apphost.ObservationRequest, revision uint64) (apphost.ObservationPage, error) {
	var page apphost.ObservationPage
	window := request.Limit
	if window == 0 {
		window = defaultAudioSeconds
	}
	if window < 1 || window > maxAudioSeconds {
		return page, apphost.ErrObservationAudioLimit
	}
	var cursor audioCursor
	if request.Cursor != "" {
		if err := json.Unmarshal([]byte(request.Cursor), &cursor); err != nil || cursor.Position < 1 || cursor.Position == math.MaxInt ||
			cursor.NextSeconds <= 0 || math.IsNaN(cursor.NextSeconds) || math.IsInf(cursor.NextSeconds, 0) || len(cursor.MediaSHA256) != 2*sha256.Size {
			return page, apphost.ErrObservationInvalid
		}
	}

	readCtx, authorize, closeScope := p.observationReadScope(ctx, request.ObservationAccess, revision)
	defer closeScope()
	if err := authorize(); err != nil {
		return page, err
	}
	version, err := p.localAudioVersion(readCtx)
	if err != nil {
		return page, err
	}
	check := func() error {
		if err := authorize(); err != nil {
			return err
		}
		current, err := p.localAudioVersion(readCtx)
		if err != nil {
			return err
		}
		if current != version {
			return apphost.ErrObservationForbidden
		}
		return nil
	}

	media, err := p.resolveMedia(readCtx, request.ContentID, maxMediaBytes)
	if authErr := check(); authErr != nil {
		return page, authErr
	}
	if err != nil {
		cause := classifyPlaybackError(err)
		if errors.Is(cause, apphost.ErrObservationPlaybackUnavailable) {
			cause = apphost.ErrObservationAudioMediaUnavailable
		}
		return page, cause
	}
	if len(media.Data) > maxMediaBytes {
		return page, apphost.ErrObservationPlaybackSize
	}
	if media.LinkOnly || len(media.Data) == 0 || media.ContentType != "video/mp4" {
		return page, apphost.ErrObservationAudioMediaUnavailable
	}
	sum := sha256.Sum256(media.Data)
	hash := hex.EncodeToString(sum[:])
	if cursor.MediaSHA256 != "" && cursor.MediaSHA256 != hash {
		return page, apphost.ErrObservationAudioSourceChanged
	}
	stage, err := os.MkdirTemp(p.privateDirectory, "audio-observation-")
	if err != nil {
		return page, apphost.ErrObservationAudioDecode
	}
	input, wavPath := filepath.Join(stage, "source.mp4"), filepath.Join(stage, "decoded.wav")
	defer func() {
		_ = os.Remove(wavPath)
		_ = os.Remove(input)
		_ = os.Remove(stage)
	}()
	if err := os.WriteFile(input, media.Data, 0o600); err != nil {
		return page, apphost.ErrObservationAudioDecode
	}
	decodeCtx, cancelDecode := context.WithTimeout(readCtx, 45*time.Second)
	defer cancelDecode()
	duration, err := mediadecode.ProbeDuration(decodeCtx, input)
	if authErr := check(); authErr != nil {
		return page, authErr
	}
	if err != nil || duration <= cursor.NextSeconds {
		return page, apphost.ErrObservationAudioDecode
	}
	end := min(duration, cursor.NextSeconds+float64(window))
	wav, decoded, err := mediadecode.ExtractAudioRange(decodeCtx, input, wavPath, cursor.NextSeconds, end-cursor.NextSeconds)
	cancelDecode()
	if authErr := check(); authErr != nil {
		return page, authErr
	}
	proof := apphost.AudioProof{
		MediaSHA256: hash, RequestedSeconds: window, StartSeconds: cursor.NextSeconds,
		WindowEndSeconds: end, DecodedSeconds: decoded, MediaDurationSeconds: duration,
	}
	page = apphost.ObservationPage{AudioProof: &proof, Complete: end == duration, Coverage: "B 站公开单段媒体 · 本地音轨读取"}
	switch {
	case errors.Is(err, mediadecode.ErrNoAudioTrack):
		proof.State, page.Complete = "no_audio_track", true
	case errors.Is(err, mediadecode.ErrAudioRangeEmpty):
		proof.State = "no_audio_in_range"
	case err != nil:
		return apphost.ObservationPage{}, apphost.ErrObservationAudioDecode
	default:
		text, err := p.transcribeAudio(readCtx, wav, check)
		if authErr := check(); authErr != nil {
			return apphost.ObservationPage{}, authErr
		}
		switch {
		case mediaspec.IsNoSpeech(err):
			proof.State = "no_recognizable_speech"
		case err != nil || strings.TrimSpace(text) == "":
			return apphost.ObservationPage{}, apphost.ErrObservationAudioTranscription
		default:
			proof.State = "transcribed"
			page.Chunks = []apphost.ObservationChunk{{Position: cursor.Position, Text: strings.TrimSpace(text)}}
		}
	}
	if !page.Complete {
		raw, err := json.Marshal(audioCursor{MediaSHA256: hash, NextSeconds: end, Position: cursor.Position + 1})
		if err != nil {
			return apphost.ObservationPage{}, apphost.ErrObservationAudioDecode
		}
		page.NextCursor = string(raw)
	}
	return page, nil
}
