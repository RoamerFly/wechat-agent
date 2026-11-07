package station

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/workspacefs"
)

var (
	errLiveProgramSource = errors.New("直播节目成品的来源、身份或文件版本不可用")
	errLiveProgramFormat = errors.New("直播节目需要实际可读取的 MP4 视频；可先用媒体工具转码再登记")
	errLiveProgramProbe  = errors.New("直播节目视频检查未完成，请检查本机解码工具或重新生成成品")
)

// Only metadata and a revocable artifact binding are cached. A probe snapshot
// is removed immediately; the encoder gets a freshly verified private copy.
type localLivePrograms struct {
	home  *localHomeService
	mu    sync.Mutex
	ref   apphost.LiveReference
	input *localProjectArtifactInput
	meta  mediadecode.Metadata
}

type localLiveProgramMedia struct {
	Source        io.ReadCloser
	SHA256        string
	Bytes         int64
	Width, Height int
}

func liveProgramSpecified(ref apphost.LiveReference) bool {
	return ref.ProgramArtifactID != "" || ref.ProgramSHA256 != "" || ref.ProgramLoop
}

func (p *localLivePrograms) check(ctx context.Context, ref apphost.LiveReference) error {
	if !liveProgramSpecified(ref) {
		return ctx.Err()
	}
	if p == nil {
		return errLiveProgramSource
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkLocked(ctx, ref)
}

func (p *localLivePrograms) checkLocked(ctx context.Context, ref apphost.LiveReference) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.home == nil || p.home.artifacts == nil || !artifactID(ref.ProgramArtifactID, "work-") || !artifactID("sha-"+ref.ProgramSHA256, "sha-") {
		return errLiveProgramSource
	}
	if p.input != nil && p.ref.ProgramArtifactID == ref.ProgramArtifactID && p.ref.ProgramSHA256 == ref.ProgramSHA256 {
		if p.input.check(ctx) != nil {
			return errLiveProgramSource
		}
		return nil
	}
	access := &localWorkspaceAccess{home: p.home}
	grant, err := access.resolve()
	if err != nil {
		return errLiveProgramSource
	}
	access.fishID, access.ownerID = grant.FishID, grant.OwnerID
	bound, err := p.home.artifacts.bindProjectArtifact(ctx, ref.ProgramArtifactID, access, grant)
	if err != nil || bound.artifact.SHA256 != ref.ProgramSHA256 {
		return errLiveProgramSource
	}
	file, err := p.snapshot(ctx, bound)
	if err != nil {
		return err
	}
	defer file.Close()
	var header [12]byte
	if _, err := io.ReadFull(file, header[:]); err != nil || string(header[4:8]) != "ftyp" {
		return errLiveProgramFormat
	}
	// Probe is local-file-only. This first implementation deliberately selects
	// MP4 rather than letting a playlist or network input reach the media source.
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	meta, err := mediadecode.ProbeMetadata(probeCtx, file.Name())
	if err != nil {
		return errLiveProgramProbe
	}
	if meta.Width < 1 || meta.Height < 1 || meta.Seconds <= 0 || meta.FPS <= 0 {
		return errLiveProgramFormat
	}
	// Decode one actual frame, not the whole video. ffprobe metadata alone is
	// insufficient, while counting every frame makes long programmes costly.
	frames, _, decodeErr := mediadecode.ExtractFramesSized(probeCtx, file.Name(), file.directory, 0, 1, 1, 64)
	removeErr := os.Remove(filepath.Join(file.directory, "frame-000001.jpg"))
	if decodeErr != nil || len(frames) != 1 || (removeErr != nil && !errors.Is(removeErr, os.ErrNotExist)) {
		return errLiveProgramProbe
	}
	if bound.check(ctx) != nil {
		return errLiveProgramSource
	}
	p.ref, p.input, p.meta = ref, bound, meta
	return nil
}

func (p *localLivePrograms) open(ctx context.Context, ref apphost.LiveReference) (*localLiveProgramMedia, error) {
	if !liveProgramSpecified(ref) {
		return nil, ctx.Err()
	}
	if p == nil {
		return nil, errLiveProgramSource
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkLocked(ctx, ref); err != nil {
		return nil, err
	}
	file, err := p.snapshot(ctx, p.input)
	if err != nil {
		return nil, err
	}
	bound := p.input
	file.verify = func() error { return bound.check(ctx) }
	return &localLiveProgramMedia{Source: file, SHA256: ref.ProgramSHA256, Bytes: p.input.artifact.Bytes,
		Width: p.meta.Width, Height: p.meta.Height}, nil
}

type localLiveProgramFile struct {
	*os.File
	directory string
	once      sync.Once
	err       error
	verify    func() error
}

func (f *localLiveProgramFile) Read(buffer []byte) (int, error) {
	n, err := f.File.Read(buffer)
	if errors.Is(err, io.EOF) && f.verify != nil && f.verify() != nil {
		return n, errLiveProgramSource
	}
	return n, err
}

func (f *localLiveProgramFile) Close() error {
	f.once.Do(func() {
		f.err = f.File.Close()
		for _, name := range []string{f.File.Name(), f.directory} {
			if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				f.err = errors.Join(f.err, err)
			}
		}
	})
	return f.err
}

func (p *localLivePrograms) snapshot(ctx context.Context, bound *localProjectArtifactInput) (*localLiveProgramFile, error) {
	directory, err := os.MkdirTemp(p.home.dataDir, "live-program-")
	if err != nil {
		return nil, errLiveProgramSource
	}
	materialized, err := bound.materialize(ctx, directory)
	if err != nil {
		_ = os.Remove(directory) // materialize removes its only partial file.
		return nil, errLiveProgramSource
	}
	root, err := workspacefs.OpenPath(directory)
	if err != nil {
		_ = os.Remove(materialized.InputPath)
		_ = os.Remove(directory)
		return nil, errLiveProgramSource
	}
	file, err := workspacefs.OpenRegular(root, filepath.Base(materialized.InputPath))
	_ = root.Close()
	if err != nil {
		_ = os.Remove(materialized.InputPath)
		_ = os.Remove(directory)
		return nil, errLiveProgramSource
	}
	return &localLiveProgramFile{File: file, directory: directory}, nil
}

func (h *localChatService) checkLiveProgramStart(ctx context.Context, in apphost.LiveStart) error {
	return h.livePrograms.check(ctx, apphost.LiveReference{ProgramArtifactID: in.ProgramArtifactID,
		ProgramSHA256: in.ProgramSHA256, ProgramLoop: in.ProgramLoop})
}
