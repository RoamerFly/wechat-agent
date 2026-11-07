package comfyui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"wechat-bot/internal/strictjson"
)

// ReferenceVersion is the protocol shipped by the current Windows portable
// installation. Process ownership and installation integrity are host duties.
const ReferenceVersion = "0.37.2"

const (
	maxJSONBytes = 4 << 20
	maxPNGBytes  = 32 << 20
	maxPNGPixels = 16 << 20
)

var (
	ErrSubmissionUnknown = errors.New("ComfyUI submission result unknown; check the original job ID")
	ErrRejected          = errors.New("ComfyUI rejected the workflow")
	ErrProtocol          = errors.New("unexpected ComfyUI response")
	ErrImageUnavailable  = errors.New("job has no verified PNG at that index")
	hostJobID            = regexp.MustCompile("^[a-f0-9]{64}$")
	saveNodeID           = regexp.MustCompile("^[a-zA-Z0-9:_-]{1,96}$")
	savedPNGName         = regexp.MustCompile(`^image_[0-9]{5,}_\.png$`)
)

type JobRef struct {
	ID       string `json:"id"`
	SaveNode string `json:"save_node"`
}

type Prepared struct {
	ref  JobRef
	body []byte
}

func (p Prepared) Ref() JobRef { return p.ref }

func (r JobRef) valid() bool {
	id, err := uuid.Parse(r.ID)
	return err == nil && id != uuid.Nil && id.String() == r.ID && saveNodeID.MatchString(r.SaveNode)
}

// NewJobRef derives the same prompt ID every time the host reopens an
// existing pluginhost job. The host job ID includes its owner and request scope.
func NewJobRef(jobID, saveNode string) (JobRef, error) {
	if !hostJobID.MatchString(jobID) || !saveNodeID.MatchString(saveNode) {
		return JobRef{}, errors.New("invalid ComfyUI host job or output node ID")
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("Agent-home-station:comfyui:"+jobID))
	return JobRef{ID: id.String(), SaveNode: saveNode}, nil
}

func (r JobRef) subfolder() string { return "station-" + r.ID }

// Prepare accepts a host-approved API workflow and scopes the approved
// SaveImage node to a private per-job output directory. The host supplies the
// approval digest from its own registry and controls every input to the graph.
func Prepare(raw []byte, approvedSHA256 string, ref JobRef) (Prepared, error) {
	return prepareSaveWorkflow(raw, approvedSHA256, ref, "SaveImage", "image")
}

func prepareSaveWorkflow(raw []byte, approvedSHA256 string, ref JobRef, class, stem string) (Prepared, error) {
	if len(raw) == 0 || len(raw) > maxJSONBytes {
		return Prepared{}, errors.New("ComfyUI workflow size outside supported range")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != approvedSHA256 {
		return Prepared{}, errors.New("ComfyUI workflow differs from approved digest")
	}
	decoded, err := strictjson.Decode(raw)
	graph, isObject := decoded.(map[string]any)
	if err != nil || !isObject {
		return Prepared{}, errors.New("expected ComfyUI API workflow JSON object")
	}
	save, _ := graph[ref.SaveNode].(map[string]any)
	inputs, _ := save["inputs"].(map[string]any)
	if !ref.valid() || inputs == nil || save["class_type"] != class {
		return Prepared{}, fmt.Errorf("approved ComfyUI output node must be %s", class)
	}
	if class == "SaveAudioAdvanced" {
		if _, exists := inputs["audio"]; !exists {
			return Prepared{}, errors.New("SaveAudioAdvanced audio input required")
		}
		// The API expects a scalar. ComfyUI expands it to a DynamicCombo
		// dictionary before executing the Python node.
		inputs["format"] = "flac"
	}
	inputs["filename_prefix"] = ref.subfolder() + "/" + stem
	body, err := json.Marshal(map[string]any{
		"prompt_id": ref.ID, "client_id": ref.ID, "prompt": graph,
		"partial_execution_targets": []string{ref.SaveNode},
	})
	if err != nil || len(body) > maxJSONBytes {
		return Prepared{}, errors.New("could not encode ComfyUI workflow submission")
	}
	return Prepared{ref: ref, body: body}, nil
}

type Client struct {
	base string
	http *http.Client
}

// NewClient connects directly to the host's verified, dedicated IPv4 loopback
// process. Redirects and system proxies cannot change the destination.
func NewClient(port uint16) (*Client, error) {
	return newClient(port, nil)
}

// NewOwnedClient is for a ComfyUI process managed by this application. verify
// must prove that the listener still belongs to that process. It is called at
// construction time and immediately before and after every new TCP dial. An
// already-established keep-alive connection remains bound to its original
// peer; a replacement listener cannot be reached through it.
func NewOwnedClient(port uint16, verify func() error) (*Client, error) {
	if verify == nil {
		return nil, errors.New("ComfyUI listener ownership verifier required")
	}
	if err := verify(); err != nil {
		return nil, fmt.Errorf("verify ComfyUI listener ownership: %w", err)
	}
	return newClient(port, verify)
}

func newClient(port uint16, verify func() error) (*Client, error) {
	if port == 0 {
		return nil, errors.New("ComfyUI loopback port required")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	dial := dialer.DialContext
	if verify != nil {
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			if err := verify(); err != nil {
				return nil, fmt.Errorf("verify ComfyUI listener ownership before dialing: %w", err)
			}
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			if err := verify(); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("verify ComfyUI listener ownership after dialing: %w", err)
			}
			return conn, nil
		}
	}
	transport := &http.Transport{
		DialContext:         dial,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 2,
	}
	return &Client{
		base: "http://127.0.0.1:" + strconv.Itoa(int(port)),
		http: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

func (c *Client) Check(ctx context.Context) error {
	return c.CheckVersion(ctx, "")
}

// CheckVersion can require the exact version pinned by the host deployment.
func (c *Client) CheckVersion(ctx context.Context, expected string) error {
	status, body, err := c.request(ctx, http.MethodGet, "/system_stats", nil, maxJSONBytes)
	if err != nil {
		return err
	}
	var response struct {
		System struct {
			Version string `json:"comfyui_version"`
		} `json:"system"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &response) != nil {
		return ErrProtocol
	}
	switch response.System.Version {
	case ReferenceVersion, "0.44.0":
		if expected != "" && response.System.Version != expected {
			return fmt.Errorf("%w: installed ComfyUI version differs from the host pin", ErrProtocol)
		}
		return nil
	default:
		return fmt.Errorf("%w: expected audited ComfyUI protocol (0.37.2 or 0.44.0)", ErrProtocol)
	}
}

// Submit issues one POST. The caller must persist and claim p.Ref() before
// calling it. A timeout, malformed receipt or mismatched ID is indeterminate.
// Resolve it with Status; the upstream queue can accept duplicate POSTs.
func (c *Client) Submit(ctx context.Context, p Prepared) error {
	if !p.ref.valid() || len(p.body) == 0 {
		return errors.New("prepared ComfyUI workflow required")
	}
	if err := c.Check(ctx); err != nil {
		return err
	}
	status, body, err := c.request(ctx, http.MethodPost, "/prompt", p.body, maxJSONBytes)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSubmissionUnknown, err)
	}
	var response struct {
		ID    string `json:"prompt_id"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &response) != nil {
		return ErrSubmissionUnknown
	}
	if status == http.StatusBadRequest && response.Error.Type != "" && response.ID == "" {
		return ErrRejected
	}
	if status != http.StatusOK || response.ID != p.ref.ID || response.Error.Type != "" {
		return ErrSubmissionUnknown
	}
	return nil
}

type State string

const (
	Unknown   State = "unknown"
	Pending   State = "pending"
	Running   State = "in_progress"
	Completed State = "completed"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

type savedFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type JobStatus struct {
	State      State
	ImageCount int
	images     []savedFile
}

func (c *Client) Status(ctx context.Context, ref JobRef) (JobStatus, error) {
	state, raw, err := c.readJob(ctx, ref)
	if err != nil {
		return JobStatus{}, err
	}
	var output struct {
		Images []savedFile `json:"images"`
	}
	if len(raw) != 0 && json.Unmarshal(raw, &output) != nil {
		return JobStatus{}, ErrProtocol
	}
	for _, image := range output.Images {
		if image.Type != "output" || image.Subfolder != ref.subfolder() || !savedPNGName.MatchString(image.Filename) {
			return JobStatus{}, ErrProtocol
		}
	}
	return JobStatus{State: state, ImageCount: len(output.Images), images: output.Images}, nil
}

func (c *Client) readJob(ctx context.Context, ref JobRef) (State, json.RawMessage, error) {
	if !ref.valid() {
		return "", nil, errors.New("invalid ComfyUI job reference")
	}
	status, body, err := c.request(ctx, http.MethodGet, "/api/jobs/"+ref.ID, nil, maxJSONBytes)
	if err != nil {
		return "", nil, err
	}
	var response struct {
		ID        string `json:"id"`
		State     State  `json:"status"`
		Error     string `json:"error"`
		Execution struct {
			Status    string `json:"status_str"`
			Completed bool   `json:"completed"`
		} `json:"execution_status"`
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "", nil, ErrProtocol
	}
	if status == http.StatusNotFound && response.Error == "Job not found" && response.ID == "" {
		return Unknown, nil, nil
	}
	if status != http.StatusOK || response.ID != ref.ID || response.Error != "" {
		return "", nil, ErrProtocol
	}
	switch response.State {
	case Pending, Running, Failed, Cancelled:
		return response.State, nil, nil
	case Completed:
		if response.Execution.Completed && response.Execution.Status == "success" {
			return Completed, response.Outputs[ref.SaveNode], nil
		}
	}
	return "", nil, ErrProtocol
}

// Cancel returns whether the backend dispatched a targeted cancellation. A
// dispatched cancellation is not a final state. The host persists intent
// before calling Cancel and checks the original job and its own intent later.
func (c *Client) Cancel(ctx context.Context, ref JobRef) (bool, error) {
	if !ref.valid() {
		return false, errors.New("invalid ComfyUI job reference")
	}
	status, body, err := c.request(ctx, http.MethodPost, "/api/jobs/"+ref.ID+"/cancel", []byte("{}"), maxJSONBytes)
	if err != nil {
		return false, err
	}
	var response struct {
		Cancelled *bool `json:"cancelled"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &response) != nil || response.Cancelled == nil {
		return false, ErrProtocol
	}
	return *response.Cancelled, nil
}

type Image struct {
	Data   []byte
	SHA256 string
	Width  int
	Height int
}

func (c *Client) FetchPNG(ctx context.Context, ref JobRef, index int) (Image, error) {
	status, err := c.Status(ctx, ref)
	if err != nil {
		return Image{}, err
	}
	if status.State != Completed || index < 0 || index >= len(status.images) {
		return Image{}, ErrImageUnavailable
	}
	image := status.images[index]
	query := url.Values{"filename": {image.Filename}, "type": {"output"}, "subfolder": {image.Subfolder}}
	code, body, err := c.request(ctx, http.MethodGet, "/view?"+query.Encode(), nil, maxPNGBytes)
	if err != nil {
		return Image{}, err
	}
	if code != http.StatusOK {
		return Image{}, ErrImageUnavailable
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxPNGPixels/cfg.Height {
		return Image{}, ErrImageUnavailable
	}
	reader := bytes.NewReader(body)
	if _, err := png.Decode(reader); err != nil || reader.Len() != 0 {
		return Image{}, ErrImageUnavailable
	}
	hash := sha256.Sum256(body)
	return Image{Data: body, SHA256: hex.EncodeToString(hash[:]), Width: cfg.Width, Height: cfg.Height}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, limit int64) (int, []byte, error) {
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	return c.requestWithContentType(ctx, method, path, body, contentType, limit)
}

func (c *Client) requestWithContentType(ctx context.Context, method, path string, body []byte, contentType string, limit int64) (int, []byte, error) {
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, input)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
		req.GetBody = nil
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(data)) > limit {
		return 0, nil, errors.New("ComfyUI response exceeds the local byte limit")
	}
	return resp.StatusCode, data, nil
}
