package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/workspacefs"
)

// localProjectArtifactInput is a short-lived, host-side authority to expose
// one immutable work artifact to one project-tool job.  It is intentionally
// not serializable: a later job must bind the artifact again and therefore
// re-check its source, owner, home grant and original bytes.
type localProjectArtifactInput struct {
	store    *localArtifactStore
	artifact localArtifact
	access   *localWorkspaceAccess
	grant    localWorkspaceGrant
}

// localProjectArtifactMaterialized is host-only job metadata.  InputPath is
// suitable for the host to mount into a single-use LPAC controller; it must
// never be exposed as a model-visible path.
type localProjectArtifactMaterialized struct {
	ArtifactID string
	Filename   string
	InputPath  string
	SHA256     string
	Bytes      int64
}

// bindProjectArtifact proves that an existing work artifact may be used as an
// input for the supplied, already-authorized home grant.  The proof is not
// durable: callers must call check at every operation boundary.
func (s *localArtifactStore) bindProjectArtifact(ctx context.Context, id string, access *localWorkspaceAccess, grant localWorkspaceGrant) (*localProjectArtifactInput, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("项目工具输入已取消")
	}
	if s == nil || access == nil || !fishidentity.ValidFishID(grant.FishID) || !fishidentity.ValidLocalOwner(fishidentity.LocalOwner{ID: grant.OwnerID, Revision: grant.Revision}) || grant.Directory == "" {
		return nil, errors.New("项目工具输入授权无效")
	}
	if !access.matches(grant.FishID, grant.OwnerID) || !access.current(grant) {
		return nil, errors.New("本机家目录授权已变更")
	}
	artifact, err := s.read(id)
	if err != nil {
		return nil, projectArtifactInputError(err)
	}
	if artifact.Fish != grant.FishID || artifact.Owner != grant.OwnerID {
		return nil, errors.New("该成品不属于当前大肥鱼家目录")
	}
	input := &localProjectArtifactInput{store: s, artifact: artifact, access: access, grant: grant}
	// Bind is the dispatch boundary: hash the original once before a caller
	// reserves a controller or job. Later controller checkpoints use check,
	// while materialize verifies the bytes again as it copies them.
	if _, err := input.current(ctx, true); err != nil {
		return nil, err
	}
	return input, nil
}

// check re-proves the binding without returning or rehashing the original host
// file. It detects forgotten or revised sources, an account/owner/home
// revision change, and altered artifact records. bindProjectArtifact and
// materialize both verify original bytes at the actual transfer boundary.
func (in *localProjectArtifactInput) check(ctx context.Context) error {
	if _, err := in.current(ctx, false); err != nil {
		return err
	}
	return nil
}

// materialize copies the proven original to exactly one fixed safe filename in
// an already-created job-private directory.  The host creates that directory;
// this method never creates or recursively removes an arbitrary caller path.
func (in *localProjectArtifactInput) materialize(ctx context.Context, directory string) (localProjectArtifactMaterialized, error) {
	if ctx == nil || ctx.Err() != nil {
		return localProjectArtifactMaterialized{}, errors.New("项目工具输入已取消")
	}
	artifact, err := in.current(ctx, false)
	if err != nil {
		return localProjectArtifactMaterialized{}, err
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return localProjectArtifactMaterialized{}, errors.New("项目工具私有目录无效")
	}
	root, err := workspacefs.OpenPath(directory)
	if err != nil {
		return localProjectArtifactMaterialized{}, errors.New("项目工具私有目录不可用")
	}
	defer root.Close()

	filename := projectArtifactInputFilename(artifact.Name)
	file, err := root.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return localProjectArtifactMaterialized{}, errors.New("本次项目工具输入已存在")
		}
		return localProjectArtifactMaterialized{}, errors.New("项目工具输入文件无法创建")
	}
	completed := false
	defer func() {
		if !completed {
			_ = file.Close()
			// A fixed filename under the already pinned job directory is the only
			// cleanup target.  Do not recursively delete a caller-controlled path.
			_ = root.Remove(filename)
		}
	}()

	allowed := func() bool {
		if ctx.Err() != nil {
			return false
		}
		// Re-check access during a potentially long copy. Source/lineage and
		// hash are checked immediately before and after the operation below.
		return in.access != nil && in.access.current(in.grant)
	}
	source, err := in.store.open(ctx, artifact, allowed)
	if err != nil {
		return localProjectArtifactMaterialized{}, projectArtifactInputError(err)
	}
	defer source.Close()
	hash := sha256.New()
	n, copyErr := io.CopyBuffer(attachmentGrantWriter{allow: allowed, out: io.MultiWriter(file, hash)}, io.LimitReader(source, artifact.Bytes+1), make([]byte, min(artifact.Bytes+1, 1<<20)))
	if copyErr != nil || n != artifact.Bytes || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 || !allowed() {
		return localProjectArtifactMaterialized{}, errors.New("项目工具输入复制未完成")
	}
	if err := file.Sync(); err != nil {
		return localProjectArtifactMaterialized{}, errors.New("项目工具输入文件无法写入")
	}
	if err := file.Close(); err != nil {
		return localProjectArtifactMaterialized{}, errors.New("项目工具输入文件无法关闭")
	}
	// The source can be forgotten/revised between source.open and this point.
	// Re-read its proof immediately before handing the fixed file to LPAC.
	if _, err := in.current(ctx, false); err != nil {
		return localProjectArtifactMaterialized{}, err
	}
	completed = true
	return localProjectArtifactMaterialized{
		ArtifactID: artifact.ID,
		Filename:   filename,
		InputPath:  filepath.Join(directory, filename),
		SHA256:     artifact.SHA256,
		Bytes:      artifact.Bytes,
	}, nil
}

// current reloads the registered record instead of trusting the values saved
// in the binding. verifyBytes additionally streams and hashes the original.
func (in *localProjectArtifactInput) current(ctx context.Context, verifyBytes bool) (localArtifact, error) {
	if ctx == nil || ctx.Err() != nil {
		return localArtifact{}, errors.New("项目工具输入已取消")
	}
	if in == nil || in.store == nil || in.access == nil || !in.access.current(in.grant) {
		return localArtifact{}, errors.New("本机家目录授权已变更")
	}
	current, err := in.store.readRecord(in.artifact.ID)
	if err != nil {
		return localArtifact{}, projectArtifactInputError(err)
	}
	if _, allowed := in.store.access(current); !allowed {
		return localArtifact{}, errors.New("已登记成品的来源、身份或版本已变更")
	}
	if !sameProjectArtifactBinding(in.artifact, current) || current.Fish != in.grant.FishID || current.Owner != in.grant.OwnerID {
		return localArtifact{}, errors.New("已登记成品的来源、身份或版本已变更")
	}
	if !verifyBytes {
		return current, nil
	}
	file, err := in.store.open(ctx, current, func() bool {
		return ctx.Err() == nil && in.access.current(in.grant) && in.store.artifactLineageValid(current)
	})
	if err != nil {
		return localArtifact{}, projectArtifactInputError(err)
	}
	return current, file.Close()
}

func sameProjectArtifactBinding(bound, current localArtifact) bool {
	if bound.ID != current.ID || bound.Name != current.Name || bound.SHA256 != current.SHA256 || bound.Fish != current.Fish || bound.Owner != current.Owner || bound.Request != current.Request || bound.PathHash != current.PathHash || bound.Bytes != current.Bytes || bound.HomeRevision != current.HomeRevision {
		return false
	}
	if (bound.Source == nil) != (current.Source == nil) || (bound.Parent == nil) != (current.Parent == nil) {
		return false
	}
	if bound.Source != nil && *bound.Source != *current.Source {
		return false
	}
	return bound.Parent == nil || *bound.Parent == *current.Parent
}

func projectArtifactInputFilename(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if len(ext) < 2 || len(ext) > 16 || !safeProjectArtifactExtension(ext) {
		ext = ""
	}
	return "input" + ext
}

func safeProjectArtifactExtension(ext string) bool {
	for _, value := range ext {
		if !(value == '.' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9') {
			return false
		}
	}
	return true
}

func projectArtifactInputError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("已登记成品无法作为项目工具输入: %w", err)
}
