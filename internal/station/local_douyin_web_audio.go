package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wechat-bot/internal/appaudio"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/stationbrowser"
	"wechat-bot/internal/workspacefs"
)

const localDouyinWebMediaMax = 32 << 20

func (h *localHomeService) localDouyinAudioVersion(ctx context.Context) (string, error) {
	if !mediadecode.Available() {
		return "", apphost.ErrObservationAudioUnavailable
	}
	return h.localStationASRVersion(ctx)
}

func (p *localDouyinWebObservation) audioBinding(ctx context.Context) (stationLocalASR, error) {
	if err := ctx.Err(); err != nil {
		return stationLocalASR{}, err
	}
	m := p.home.douyinWeb
	m.mu.Lock()
	audio := m.audio
	m.mu.Unlock()
	if audio.version == nil || audio.transcribe == nil {
		return stationLocalASR{}, apphost.ErrObservationAudioUnavailable
	}
	return audio, nil
}

func (p *localDouyinWebObservation) readAudio(ctx context.Context, a localDouyinWebAccess, in apphost.ObservationRequest, target localDouyinWebTarget, authorize func() error) (apphost.ObservationPage, error) {
	if in.Limit > 240 {
		return apphost.ObservationPage{}, apphost.ErrObservationAudioLimit
	}
	audio, err := p.audioBinding(ctx)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	version, err := audio.ready(ctx)
	if err != nil || strings.TrimSpace(version) == "" {
		return apphost.ObservationPage{}, apphost.ErrObservationAudioUnavailable
	}
	check := func() error {
		if err := authorize(); err != nil {
			return err
		}
		v, err := audio.ready(ctx)
		if err != nil || v != version {
			return apphost.ErrObservationForbidden
		}
		return nil
	}
	if err := check(); err != nil {
		return apphost.ObservationPage{}, err
	}
	media, hash, err := p.originalMedia(ctx, a, in, target, check)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	page, err := appaudio.Read(ctx, appaudio.Request{Media: media.Data, ContentType: media.ContentType, PrivateDirectory: p.snapshotDirectory(a), Cursor: in.Cursor, Limit: in.Limit, Coverage: "抖音确切视频原件 · 本机音轨转写；不代表画面理解或视频播放", SourceID: hash, SourceURL: target.url}, check, audio.transcribe)
	if authErr := check(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	return page, err
}

type localDouyinWebMediaRecord struct {
	Version     int    `json:"schema_version"`
	ContentID   string `json:"content_id"`
	Grant       string `json:"grant"`
	MediaSHA256 string `json:"media_sha256"`
	ContentType string `json:"content_type"`
}

// Raw media stays in the host's private per-grant scope, never the editable
// home or model result. Continuations decode the same verified bytes rather
// than downloading a potentially different video for each audio or visual page.
func (p *localDouyinWebObservation) originalMedia(ctx context.Context, a localDouyinWebAccess, in apphost.ObservationRequest, target localDouyinWebTarget, authorize func() error) (stationbrowser.DouyinWebMedia, string, error) {
	if target.videoID == "" {
		return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationInvalid
	}
	nameSum := sha256.Sum256([]byte(in.ContentID))
	name := "audio-" + hex.EncodeToString(nameSum[:])
	dir := p.snapshotDirectory(a)
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationWebStorageUnavailable
	}
	defer root.Close()
	release, err := hostfile.TryLock(filepath.Join(dir, name+".lock"))
	if err != nil {
		return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationWebSessionBusy
	}
	defer release()
	if !in.Refresh {
		f, openErr := workspacefs.OpenRegular(root, name+".json")
		if openErr == nil {
			raw, readErr := io.ReadAll(io.LimitReader(f, 4097))
			f.Close()
			var meta localDouyinWebMediaRecord
			if readErr != nil || len(raw) > 4096 || strictLocalMemoryDecode(raw, &meta) != nil || meta.Version != 1 || meta.Grant != a.version || meta.ContentID != in.ContentID {
				return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationWebStorageUnavailable
			}
			dataFile, e := workspacefs.OpenRegular(root, name+".media")
			if e != nil {
				return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationWebStorageUnavailable
			}
			data, e := io.ReadAll(io.LimitReader(dataFile, localDouyinWebMediaMax+1))
			dataFile.Close()
			sum := sha256.Sum256(data)
			hash := hex.EncodeToString(sum[:])
			if e != nil || len(data) == 0 || len(data) > localDouyinWebMediaMax || hash != meta.MediaSHA256 {
				return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationWebStorageUnavailable
			}
			if err := authorize(); err != nil {
				return stationbrowser.DouyinWebMedia{}, "", err
			}
			return stationbrowser.DouyinWebMedia{Data: data, ContentType: meta.ContentType}, hash, nil
		}
		if !errors.Is(openErr, os.ErrNotExist) {
			return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationWebStorageUnavailable
		}
		if in.Cursor != "" {
			return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationAudioSourceChanged
		}
	}
	m := p.home.douyinWeb
	m.mu.Lock()
	read, serviceCtx := m.readMedia, m.ctx
	m.mu.Unlock()
	if read == nil {
		return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationBrowserUnavailable
	}
	readCtx, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	stop := context.AfterFunc(serviceCtx, cancel)
	defer stop()
	media, err := read(readCtx, a.profile, target.videoID, authorize)
	if authErr := authorize(); authErr != nil {
		return stationbrowser.DouyinWebMedia{}, "", authErr
	}
	if err != nil {
		if errors.Is(err, stationbrowser.ErrDouyinWebMediaTooLarge) {
			return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationPlaybackSize
		}
		if errors.Is(err, stationbrowser.ErrDouyinWebMediaUnavailable) {
			return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationAudioMediaUnavailable
		}
		return stationbrowser.DouyinWebMedia{}, "", localDouyinWebReadError(err)
	}
	if len(media.Data) > localDouyinWebMediaMax {
		return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationPlaybackSize
	}
	media.ContentType = strings.ToLower(strings.TrimSpace(strings.Split(media.ContentType, ";")[0]))
	if len(media.Data) == 0 || media.ContentType != "video/mp4" && media.ContentType != "video/webm" {
		return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationAudioMediaUnavailable
	}
	sum := sha256.Sum256(media.Data)
	hash := hex.EncodeToString(sum[:])
	raw, err := json.Marshal(localDouyinWebMediaRecord{Version: 1, ContentID: in.ContentID, Grant: a.version, MediaSHA256: hash, ContentType: media.ContentType})
	if err != nil || hostfile.WritePrivate(filepath.Join(dir, name+".media"), media.Data) != nil || hostfile.WritePrivate(filepath.Join(dir, name+".json"), raw) != nil {
		return stationbrowser.DouyinWebMedia{}, "", apphost.ErrObservationWebStorageUnavailable
	}
	if err := authorize(); err != nil {
		return stationbrowser.DouyinWebMedia{}, "", err
	}
	return media, hash, nil
}
