//go:build windows

package obsportable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

const programMedia = "config/obs-studio/program.mp4"

type ownedMedia struct {
	Media
	once     sync.Once
	closeErr error
}

func (m *ownedMedia) close() error {
	m.once.Do(func() {
		if m.Source != nil {
			m.closeErr = m.Source.Close()
		}
	})
	return m.closeErr
}

func writeProfileWithMedia(ctx context.Context, root *os.Root, studio string, port int, ticket string, media *ownedMedia) error {
	if err := writeProfile(root, port, ticket); err != nil {
		return err
	}
	if media == nil {
		return nil
	}
	// writeProfile removes the preceding private config before this copy. It
	// never changes the immutable runtime inventory verified by Installation.
	if err := copyProgramMedia(ctx, root, media); err != nil {
		return err
	}
	scene, err := mediaSceneCollection(filepath.Join(studio, filepath.FromSlash(programMedia)), media)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return root.WriteFile(filepath.FromSlash("config/obs-studio/basic/scenes/"+profileName+".json"), scene, 0600)
}

type contextMediaReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextMediaReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if cancel := r.ctx.Err(); cancel != nil {
		return n, cancel
	}
	return n, err
}

func copyProgramMedia(ctx context.Context, root *os.Root, media *ownedMedia) (result error) {
	// Close lets the host's snapshot stream interrupt a blocked Read on cancel.
	stopCancellation := context.AfterFunc(ctx, func() { _ = media.close() })
	created := false
	defer func() {
		stopCancellation()
		result = errors.Join(result, media.close(), ctx.Err())
		if result != nil && created {
			result = errors.Join(result, root.Remove(filepath.FromSlash(programMedia)))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := root.OpenFile(filepath.FromSlash(programMedia), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	created = true
	defer func() { result = errors.Join(result, f.Close()) }()
	hash := sha256.New()
	reader := contextMediaReader{ctx: ctx, source: media.Source}
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(reader, media.Bytes))
	if err != nil {
		return err
	}
	if n != media.Bytes || hex.EncodeToString(hash.Sum(nil)) != media.SHA256 {
		return ErrInvalid
	}
	var extra [1]byte
	if n, err := io.ReadFull(reader, extra[:]); n != 0 || err != io.EOF {
		return errors.Join(ErrInvalid, err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return ctx.Err()
}

func mediaSceneCollection(localFile string, media *ownedMedia) ([]byte, error) {
	var collection map[string]any
	if err := json.Unmarshal([]byte(sceneCollection), &collection); err != nil {
		return nil, err
	}
	scale := math.Min(1280/float64(media.Width), 720/float64(media.Height))
	sources := collection["sources"].([]any)
	const mediaUUID = "b87bf589-a26d-47cb-87ae-22d647fc572a"
	// OBS 33.0.1 obs-ffmpeg-source.c reads these exact keys. Local mode
	// ignores input_format, so the fixed FFmpeg demuxer whitelist is supplied
	// through its supported ffmpeg_options setting instead. Callers cannot
	// inject options, URLs, captures, scripts or other source types.
	// https://github.com/obsproject/obs-studio/blob/33.0.1/plugins/obs-ffmpeg/obs-ffmpeg-source.c
	// https://github.com/obsproject/obs-studio/blob/33.0.1/shared/media-playback/media-playback/media.c
	mediaSource := map[string]any{
		"id": "ffmpeg_source", "versioned_id": "ffmpeg_source", "name": "Station Media", "uuid": mediaUUID,
		"enabled": true, "muted": false, "volume": 1.0, "mixers": 1, "monitoring_type": 0,
		"settings": map[string]any{
			"is_local_file": true, "local_file": localFile, "looping": media.Loop,
			"restart_on_activate": true, "clear_on_media_end": true, "close_when_inactive": false,
			"hw_decode": false, "speed_percent": 100,
			"ffmpeg_options": "protocol_whitelist=file,pipe format_whitelist=mov",
		},
	}
	items := sources[1].(map[string]any)["settings"].(map[string]any)["items"].([]any)
	items = append(items, map[string]any{
		"id": 2, "name": "Station Media", "source_uuid": mediaUUID, "visible": true,
		"align": 5, "bounds_type": 0, "rot": 0,
		"pos":   map[string]float64{"x": (1280 - float64(media.Width)*scale) / 2, "y": (720 - float64(media.Height)*scale) / 2},
		"scale": map[string]float64{"x": scale, "y": scale},
	})
	sources[1].(map[string]any)["settings"].(map[string]any)["items"] = items
	// Declare component sources before the scene that references them.
	collection["sources"] = []any{sources[0], mediaSource, sources[1]}
	return json.Marshal(collection)
}
