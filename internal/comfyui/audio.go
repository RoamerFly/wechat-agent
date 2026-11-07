package comfyui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
)

const (
	maxWAVBytes  = 64 << 20
	maxFLACBytes = 256 << 20
)

var (
	ErrAudioUnavailable = errors.New("job has no verified FLAC output")
	savedFLACName       = regexp.MustCompile(`^song_00001\.flac$`)
)

// FLAC is an output produced by the exact SaveAudioAdvanced node selected by a
// JobRef. Data has been bounded, checked for a FLAC stream marker, and hashed.
type FLAC struct {
	Data     []byte
	SHA256   string
	Filename string
}

// AudioStatus is deliberately separate from JobStatus. It proves that the
// successful job exposed audio at this JobRef's output node, rather than
// treating an unrelated node or an image-only result as an audio completion.
type AudioStatus struct {
	State      State
	AudioCount int
	audio      []savedFile
}

// PrepareFLAC scopes an already-approved YuE workflow to one private output
// folder. Although SaveAudioAdvanced receives a DynamicCombo dictionary in
// Python, the audited 0.37.2 API workflow wire format is the scalar "flac";
// ComfyUI expands it before node execution.
func PrepareFLAC(raw []byte, approvedSHA256 string, ref JobRef) (Prepared, error) {
	return prepareSaveWorkflow(raw, approvedSHA256, ref, "SaveAudioAdvanced", "song")
}

// AudioStatus only accepts a completed backend job when ComfyUI's independent
// execution proof is success. No output item from another node is considered.
func (c *Client) AudioStatus(ctx context.Context, ref JobRef) (AudioStatus, error) {
	state, raw, err := c.readJob(ctx, ref)
	if err != nil {
		return AudioStatus{}, err
	}
	if state != Completed {
		return AudioStatus{State: state}, nil
	}
	var output struct {
		Audio []savedFile `json:"audio"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &output) != nil {
		return AudioStatus{}, ErrProtocol
	}
	for _, audio := range output.Audio {
		if audio.Type != "output" || audio.Subfolder != ref.subfolder() || !savedFLACName.MatchString(audio.Filename) {
			return AudioStatus{}, ErrProtocol
		}
	}
	return AudioStatus{State: state, AudioCount: len(output.Audio), audio: output.Audio}, nil
}

// FetchFLAC reads exactly one output of the expected successful job. It never
// interpolates a model-provided path into the request.
func (c *Client) FetchFLAC(ctx context.Context, ref JobRef) (FLAC, error) {
	status, err := c.AudioStatus(ctx, ref)
	if err != nil {
		return FLAC{}, err
	}
	if status.State != Completed || len(status.audio) != 1 {
		return FLAC{}, ErrAudioUnavailable
	}
	audio := status.audio[0]
	query := url.Values{"filename": {audio.Filename}, "type": {"output"}, "subfolder": {audio.Subfolder}}
	code, body, err := c.request(ctx, http.MethodGet, "/view?"+query.Encode(), nil, maxFLACBytes)
	if err != nil {
		return FLAC{}, err
	}
	if code != http.StatusOK || len(body) < 4 || !bytes.Equal(body[:4], []byte("fLaC")) {
		return FLAC{}, ErrAudioUnavailable
	}
	hash := sha256.Sum256(body)
	return FLAC{Data: body, SHA256: hex.EncodeToString(hash[:]), Filename: audio.Filename}, nil
}

// UploadWAV places one private reference WAV in the managed ComfyUI input
// root. The current 0.37.2 server has no /upload/audio route: its audited
// /upload/image handler is generic, so this method uses that route while
// retaining its field name. Ref-derived names and an empty subfolder prevent
// callers from selecting arbitrary input paths.
func (c *Client) UploadWAV(ctx context.Context, ref JobRef, wav []byte) (string, error) {
	filename, err := ReferenceWAVName(ref)
	if err != nil {
		return "", err
	}
	if len(wav) < 12 || len(wav) > maxWAVBytes || !bytes.Equal(wav[:4], []byte("RIFF")) || !bytes.Equal(wav[8:12], []byte("WAVE")) {
		return "", errors.New("reference must be a bounded RIFF/WAVE file")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	if err := writer.WriteField("type", "input"); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	code, response, err := c.requestWithContentType(ctx, http.MethodPost, "/upload/image", body.Bytes(), writer.FormDataContentType(), maxJSONBytes)
	if err != nil {
		return "", err
	}
	var uploaded struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
		Type      string `json:"type"`
	}
	if code != http.StatusOK || json.Unmarshal(response, &uploaded) != nil || uploaded.Name != filename || uploaded.Subfolder != "" || uploaded.Type != "input" {
		return "", fmt.Errorf("%w: reference WAV upload receipt did not match managed input", ErrProtocol)
	}
	return filename, nil
}

// ReferenceWAVName lets the host bind a prepared recipe to the same name the
// upload protocol uses, before it claims the job or writes any backend input.
func ReferenceWAVName(ref JobRef) (string, error) {
	if !ref.valid() {
		return "", errors.New("invalid ComfyUI job reference")
	}
	return "station-" + ref.ID + ".wav", nil
}

// YuERequiredNodeSchemas fetches only the three node schemas permitted to
// influence the recipe compiler. Returning a merged raw object avoids exposing
// the rest of /object_info to prompts or plugin code.
func (c *Client) YuERequiredNodeSchemas(ctx context.Context) (json.RawMessage, error) {
	allowed := []string{"YuE2GenerateMusic", "YuE2GenerateABC", "KSampler"}
	merged := make(map[string]json.RawMessage, len(allowed))
	for _, name := range allowed {
		code, body, err := c.request(ctx, http.MethodGet, "/object_info/"+url.PathEscape(name), nil, maxJSONBytes)
		if err != nil {
			return nil, err
		}
		var response map[string]json.RawMessage
		if code != http.StatusOK || json.Unmarshal(body, &response) != nil || len(response) != 1 || len(response[name]) == 0 {
			return nil, ErrProtocol
		}
		var schema struct {
			Input struct {
				Required json.RawMessage `json:"required"`
			} `json:"input"`
		}
		if json.Unmarshal(response[name], &schema) != nil || len(schema.Input.Required) == 0 || !json.Valid(schema.Input.Required) {
			return nil, ErrProtocol
		}
		merged[name] = response[name]
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}
