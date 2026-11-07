package linuxnative

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// IncomingMedia obtains an owned message's attachment from the native transfer
// API. It never lets a message choose a local filesystem path or bearer URL.
func (s *Sender) incomingMediaResponse(ctx context.Context, messageID string, max int64, quality string) (*http.Response, error) {
	return s.incomingMediaResponseChecked(ctx, messageID, max, quality, nil)
}

func (s *Sender) incomingMediaResponseChecked(ctx context.Context, messageID string, max int64, quality string, checkAsset func(string) error) (*http.Response, error) {
	if len(messageID) == 0 || len(messageID) > 8192 || max < 1 || max > 512<<20 {
		return nil, errors.New("invalid incoming media request")
	}
	switch quality {
	case "best", "normal", "original", "standard", "encoded":
	default:
		return nil, errors.New("invalid incoming media quality")
	}
	prefix := s.accountPath() + "/messages/" + url.PathEscape(messageID)
	body, _ := json.Marshal(map[string]string{"messageId": messageID, "quality": quality})
	raw, err := s.toolJSON(ctx, "POST", prefix+"/media-transfers", "", bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, err
	}
	type mediaJob struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		AssetID string `json:"assetId"`
		Account string `json:"accountId"`
		Kind    string `json:"kind"`
	}
	var job mediaJob
	validJob := func() bool {
		return safeID(job.ID) && (checkAsset == nil || job.Account == s.account && job.Kind == "download")
	}
	if json.Unmarshal(raw, &job) != nil || !validJob() {
		return nil, errors.New("invalid incoming media job")
	}
	jobID, assetID := job.ID, job.AssetID
	for job.State != "completed" && job.State != "succeeded" && job.State != "available" {
		if job.State == "failed" || job.State == "cancelled" || job.State == "needs_review" {
			return nil, errors.New("incoming media unavailable")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		raw, err = s.toolJSON(ctx, "GET", s.accountPath()+"/jobs/"+url.PathEscape(jobID), "", nil, "")
		if err != nil {
			return nil, err
		}
		job = mediaJob{}
		if err := json.Unmarshal(raw, &job); err != nil || !validJob() || job.ID != jobID || assetID != "" && job.AssetID != assetID {
			return nil, errors.New("invalid media job")
		}
		assetID = job.AssetID
	}
	if !safeID(job.AssetID) {
		return nil, errors.New("media asset missing")
	}
	if checkAsset != nil {
		if err := checkAsset(job.AssetID); err != nil {
			return nil, err
		}
	}
	req, err := s.newRequest(ctx, "GET", s.accountPath()+"/media/"+url.PathEscape(job.AssetID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.New("media_content_unavailable")
	}
	return resp, nil
}

// VerifiedIncomingMedia is the native metadata verified against the original stream.
type VerifiedIncomingMedia struct {
	Bytes   int64
	MIME    string
	SHA256  string
	Variant string
}

type incomingMediaAsset struct {
	ID      string `json:"id"`
	Account string `json:"accountId"`
	Message string `json:"messageId"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Variant string `json:"variant"`
	Actual  string `json:"actualVariant"`
	Bytes   int64  `json:"byteLength"`
	MIME    string `json:"contentType"`
	SHA256  string `json:"sha256"`
}

func (s *Sender) incomingMediaSourceVariant(ctx context.Context, message, kind string) (string, error) {
	var alternative string
	switch kind {
	case "file":
		return "original", nil
	case "voice":
		return "encoded", nil
	case "image":
		alternative = "standard"
	case "video":
		alternative = "normal"
	default:
		return "", errors.New("unsupported incoming media kind")
	}
	var assets struct {
		Items []incomingMediaAsset `json:"items"`
	}
	if err := s.readJSON(ctx, s.accountPath()+"/messages/"+url.PathEscape(message)+"/media", &assets); err != nil {
		return "", err
	}
	for _, candidate := range []string{"original", alternative} {
		for _, asset := range assets.Items {
			if asset.Account == s.account && asset.Message == message && asset.Kind == kind && asset.Variant == candidate &&
				asset.State != "expired" && asset.State != "unsupported" && (asset.State != "available" || asset.Actual == candidate) {
				return candidate, nil
			}
		}
	}
	return "", errors.New("incoming_media_full_source_unavailable")
}

// StreamVerifiedIncomingMedia binds the original to the native message and
// declared asset metadata. The caller publishes the staged output only on success.
func (s *Sender) StreamVerifiedIncomingMedia(ctx context.Context, messageID, kind string, max int64, output io.Writer) (VerifiedIncomingMedia, error) {
	if len(messageID) == 0 || len(messageID) > 8192 || max < 1 || max > 512<<20 || output == nil {
		return VerifiedIncomingMedia{}, errors.New("invalid incoming media request")
	}
	variant, err := s.incomingMediaSourceVariant(ctx, messageID, kind)
	if err != nil {
		return VerifiedIncomingMedia{}, err
	}
	return s.streamVerifiedIncomingMedia(ctx, messageID, kind, variant, max, output)
}

// StreamVerifiedPlayableVoice verifies the separately decoded native voice
// rendition. The encoded original is still acquired by the attachment API.
func (s *Sender) StreamVerifiedPlayableVoice(ctx context.Context, messageID string, max int64, output io.Writer) (VerifiedIncomingMedia, error) {
	if output == nil {
		return VerifiedIncomingMedia{}, errors.New("invalid incoming media request")
	}
	return s.streamVerifiedIncomingMedia(ctx, messageID, "voice", "normal", max, output)
}

func (s *Sender) streamVerifiedIncomingMedia(ctx context.Context, messageID, kind, variant string, max int64, output io.Writer) (VerifiedIncomingMedia, error) {
	var expected VerifiedIncomingMedia
	resp, err := s.incomingMediaResponseChecked(ctx, messageID, max, variant, func(id string) error {
		var asset incomingMediaAsset
		err := s.readJSON(ctx, s.accountPath()+"/media/"+url.PathEscape(id), &asset)
		hash, hashErr := hex.DecodeString(asset.SHA256)
		if err != nil || asset.ID != id || asset.Account != s.account || asset.Message != messageID || asset.Kind != kind ||
			asset.State != "available" || asset.Variant != variant || asset.Actual != variant || asset.Bytes <= 0 || asset.Bytes > max || hashErr != nil || len(hash) != sha256.Size {
			return errors.New("incoming_media_asset_identity_unverified")
		}
		expected = VerifiedIncomingMedia{Bytes: asset.Bytes, MIME: asset.MIME, SHA256: hex.EncodeToString(hash), Variant: variant}
		return nil
	})
	if err != nil {
		return VerifiedIncomingMedia{}, err
	}
	defer resp.Body.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(resp.Body, expected.Bytes+1))
	if err != nil {
		return VerifiedIncomingMedia{}, err
	}
	if n != expected.Bytes || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return VerifiedIncomingMedia{}, errors.New("incoming_media_integrity_mismatch")
	}
	return expected, nil
}

func (s *Sender) IncomingMedia(ctx context.Context, messageID string, max int64) ([]byte, string, error) {
	resp, err := s.incomingMediaResponse(ctx, messageID, max, "best")
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || len(data) == 0 || int64(len(data)) > max {
		return nil, "", errors.New("incoming_media_exceeds_limit")
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// IncomingVoiceMedia asks the native bridge for its normal/playable voice
// rendition.  Raw WeChat SILK is retained only when that bridge cannot supply
// normal media; callers must report that condition instead of treating it as a
// decodable training or ASR input.
func (s *Sender) IncomingVoiceMedia(ctx context.Context, messageID string, max int64) ([]byte, string, error) {
	resp, err := s.incomingMediaResponse(ctx, messageID, max, "normal")
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || len(data) == 0 || int64(len(data)) > max {
		return nil, "", errors.New("incoming_media_exceeds_limit")
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// IncomingMediaTo streams a native-owned transfer to an exclusively created
// private file. The caller owns removal on failure; partial files are never assets.
func (s *Sender) IncomingMediaTo(ctx context.Context, messageID, path string, max int64) (int64, string, error) {
	resp, err := s.incomingMediaResponse(ctx, messageID, max, "best")
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, "", err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if err == nil && n > 0 && n <= max {
		err = f.Sync()
		if err == nil {
			err = f.Close()
		}
		if err == nil {
			ok = true
			return n, resp.Header.Get("Content-Type"), nil
		}
	}
	if err == nil {
		err = errors.New("incoming_media_size_invalid")
	}
	return n, "", err
}
