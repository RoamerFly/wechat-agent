package station

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/stationbrowser"
	"wechat-bot/internal/workspacefs"
)

// Only a successful, authorized read can issue this identity/version proof.
// Keeping the text out avoids duplicating the observation cache in the ledger.
type localDouyinCommentProof struct {
	VideoID   string `json:"video_id"`
	CommentID string `json:"comment_id"`
	AuthorID  string `json:"author_id"`
	SHA256    string `json:"sha256"`
	Grant     string `json:"grant"`
}

func (p *localDouyinWebObservation) saveCommentProof(a localDouyinWebAccess, video string, comment stationbrowser.DouyinComment) error {
	root, err := workspacefs.EnsurePath(filepath.Join(p.snapshotDirectory(a), "comments", video))
	if err != nil {
		return apphost.ErrObservationWebStorageUnavailable
	}
	root.Close()
	sum := sha256.Sum256([]byte(comment.Text))
	proof := localDouyinCommentProof{VideoID: video, CommentID: comment.ID, AuthorID: comment.AuthorID, SHA256: hex.EncodeToString(sum[:]), Grant: a.version}
	raw, _ := json.Marshal(proof)
	if err := hostfile.WritePrivate(filepath.Join(p.snapshotDirectory(a), "comments", video, comment.ID+".json"), raw); err != nil {
		return apphost.ErrObservationWebStorageUnavailable
	}
	return nil
}

func (p *localDouyinWebObservation) commentProof(a localDouyinWebAccess, video, id string) (localDouyinCommentProof, error) {
	var proof localDouyinCommentProof
	if !validLocalDouyinCommentID(id) {
		return proof, apphost.ErrObservationForbidden
	}
	root, err := workspacefs.OpenPath(filepath.Join(p.snapshotDirectory(a), "comments", video))
	if err != nil {
		return proof, apphost.ErrObservationForbidden
	}
	defer root.Close()
	f, err := workspacefs.OpenRegular(root, id+".json")
	if err != nil {
		return proof, apphost.ErrObservationForbidden
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 || strictLocalMemoryDecode(raw, &proof) != nil || proof.VideoID != video || proof.CommentID != id || proof.AuthorID == "" || proof.Grant != a.version || !localDouyinPublicationHex(proof.SHA256) {
		return proof, apphost.ErrObservationForbidden
	}
	return proof, nil
}
