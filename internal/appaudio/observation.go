// Package appaudio turns an authorized local media object into paginated audio
// evidence. Application identity, fetching, caching and grants stay with the
// caller; this package never opens a remote URL or selects a recognition model.
package appaudio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/workspacefs"
)

const (
	MaxMediaBytes  = 32 << 20
	DefaultSeconds = 60
	// At 16 kHz mono PCM16 this window fits mediadecode's 8 MiB WAV bound.
	MaxSeconds = 240
)

// Request contains bytes already associated with the exact application item.
// PrivateDirectory must be an existing absolute host-private directory, outside
// the editable fish home. SourceURL is public provenance, never a signed media
// URL. Limit is seconds, and Cursor is opaque host-retained continuation state.
type Request struct {
	Media            []byte
	ContentType      string
	PrivateDirectory string
	Cursor           string
	Limit            int
	Coverage         string
	SourceID         string
	SourceURL        string
}

type audioCursor struct {
	MediaSHA256 string  `json:"media_sha256"`
	NextSeconds float64 `json:"next_seconds"`
	Position    int     `json:"position"`
}

// Read decodes exactly one window and returns only verified evidence. A media
// object with no audio, or a recognizer's explicit no-speech result, is a valid
// outcome distinct from decoder/recognizer failure. Authorization or context
// cancellation wins even when a later stage appears to have succeeded.
func Read(ctx context.Context, request Request, authorize func() error, transcribe func(context.Context, []byte, func() error) (string, error)) (apphost.ObservationPage, error) {
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if authorize == nil {
			return apphost.ErrObservationForbidden
		}
		return authorize()
	}
	if err := check(); err != nil {
		return apphost.ObservationPage{}, err
	}
	window := request.Limit
	if window == 0 {
		window = DefaultSeconds
	}
	if window < 1 || window > MaxSeconds {
		return apphost.ObservationPage{}, apphost.ErrObservationAudioLimit
	}
	if len(request.Media) > MaxMediaBytes {
		return apphost.ObservationPage{}, apphost.ErrObservationPlaybackSize
	}
	contentType, _, err := mime.ParseMediaType(request.ContentType)
	if err != nil || len(request.Media) == 0 || contentType != "video/mp4" && contentType != "video/webm" {
		return apphost.ObservationPage{}, apphost.ErrObservationAudioMediaUnavailable
	}
	if transcribe == nil || !mediadecode.Available() {
		return apphost.ObservationPage{}, apphost.ErrObservationAudioUnavailable
	}
	cursor, err := parseCursor(request.Cursor)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	sum := sha256.Sum256(request.Media)
	hash := hex.EncodeToString(sum[:])
	if cursor.MediaSHA256 != "" && cursor.MediaSHA256 != hash {
		return apphost.ObservationPage{}, apphost.ErrObservationAudioSourceChanged
	}
	if !filepath.IsAbs(request.PrivateDirectory) {
		return apphost.ObservationPage{}, apphost.ErrObservationWebStorageUnavailable
	}
	root, err := workspacefs.OpenPath(request.PrivateDirectory)
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationWebStorageUnavailable
	}
	defer root.Close()
	if err := check(); err != nil {
		return apphost.ObservationPage{}, err
	}
	stage, err := os.MkdirTemp(request.PrivateDirectory, "audio-observation-")
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationWebStorageUnavailable
	}
	// Cleanup is rooted in the directory checked above, never a string-built
	// shell operation or a recursively followed link from the media contents.
	defer func() { _ = root.RemoveAll(filepath.Base(stage)) }()
	input, wavPath := filepath.Join(stage, "source.mp4"), filepath.Join(stage, "decoded.wav")
	file, err := root.OpenFile(filepath.Join(filepath.Base(stage), "source.mp4"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationWebStorageUnavailable
	}
	_, writeErr := file.Write(request.Media)
	closeErr := file.Close()
	if err := check(); err != nil {
		return apphost.ObservationPage{}, err
	}
	if writeErr != nil || closeErr != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationWebStorageUnavailable
	}
	decodeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	duration, err := mediadecode.ProbeDuration(decodeCtx, input)
	if authErr := check(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationAudioDecode
	}
	if duration <= cursor.NextSeconds {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	end := min(duration, cursor.NextSeconds+float64(window))
	wav, decoded, decodeErr := mediadecode.ExtractAudioRange(decodeCtx, input, wavPath, cursor.NextSeconds, end-cursor.NextSeconds)
	cancel()
	if authErr := check(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	proof := apphost.AudioProof{
		MediaSHA256: hash, RequestedSeconds: window, StartSeconds: cursor.NextSeconds,
		WindowEndSeconds: end, DecodedSeconds: decoded, MediaDurationSeconds: duration,
	}
	coverage := request.Coverage
	if coverage == "" {
		coverage = "本机音轨读取"
	}
	page := apphost.ObservationPage{AudioProof: &proof, Complete: end == duration, Coverage: coverage}
	switch {
	case errors.Is(decodeErr, mediadecode.ErrNoAudioTrack):
		proof.State, page.Complete = "no_audio_track", true
	case errors.Is(decodeErr, mediadecode.ErrAudioRangeEmpty):
		proof.State = "no_audio_in_range"
	case decodeErr != nil:
		return apphost.ObservationPage{}, apphost.ErrObservationAudioDecode
	default:
		text, err := transcribe(ctx, wav, check)
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
			page.Chunks = []apphost.ObservationChunk{{Position: cursor.Position, Text: strings.TrimSpace(text), SourceID: request.SourceID, SourceURL: request.SourceURL}}
		}
	}
	if !page.Complete {
		raw, err := json.Marshal(audioCursor{MediaSHA256: hash, NextSeconds: end, Position: cursor.Position + 1})
		if err != nil {
			return apphost.ObservationPage{}, apphost.ErrObservationAudioDecode
		}
		page.NextCursor = string(raw)
	}
	if err := check(); err != nil {
		return apphost.ObservationPage{}, err
	}
	return page, nil
}

func parseCursor(raw string) (audioCursor, error) {
	var cursor audioCursor
	if raw == "" {
		return cursor, nil
	}
	if len(raw) > 512 {
		return cursor, apphost.ErrObservationInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil {
		return cursor, apphost.ErrObservationInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || cursor.Position < 1 || cursor.Position == math.MaxInt ||
		cursor.NextSeconds <= 0 || math.IsNaN(cursor.NextSeconds) || math.IsInf(cursor.NextSeconds, 0) {
		return cursor, apphost.ErrObservationInvalid
	}
	digest, err := hex.DecodeString(cursor.MediaSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != cursor.MediaSHA256 {
		return cursor, apphost.ErrObservationInvalid
	}
	return cursor, nil
}
