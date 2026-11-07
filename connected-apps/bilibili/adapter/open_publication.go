package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// ArchiveMetadata is the caller-supplied, non-media part of an archive
// submission. The upload token passed to Post is deliberately kept separate:
// it is an opaque capability, not metadata that can be accidentally logged or
// persisted with a publication request.
type ArchiveMetadata struct {
	Title       string
	Description string
	CategoryID  int
	Tags        []string
	Copyright   int
	NoReprint   bool
	Source      string
}

// Validate checks the fields that this adapter can establish locally. It does
// not attempt to predict platform review rules or infer an unlisted category.
func (m ArchiveMetadata) Validate() error {
	if strings.TrimSpace(m.Title) == "" || containsArchiveControl(m.Title) || m.CategoryID <= 0 || len(m.Tags) == 0 {
		return ErrOpenUploadInvalid
	}
	if m.Copyright != 1 && m.Copyright != 2 {
		return ErrOpenUploadInvalid
	}
	for _, tag := range m.Tags {
		// The Open API represents tags as one comma-delimited string. A comma in
		// one element would silently turn a caller's one tag into several tags.
		if strings.TrimSpace(tag) == "" || strings.ContainsRune(tag, ',') || containsArchiveControl(tag) {
			return ErrOpenUploadInvalid
		}
	}
	if containsArchiveControl(m.Source) || containsArchiveControl(m.Description) {
		return ErrOpenUploadInvalid
	}
	// Reprint submissions have a source field in the documented request shape;
	// require it rather than sending an ambiguous reprint declaration.
	if m.Copyright == 2 && strings.TrimSpace(m.Source) == "" {
		return ErrOpenUploadInvalid
	}
	return nil
}

// ArchiveStatus only reports the state that is confirmed by the documented
// response example. Other provider states are intentionally not translated
// into a local lifecycle vocabulary.
type ArchiveStatus struct {
	Published bool
}

// Post submits metadata for an already completed upload token and returns the
// opaque resource ID allocated by the Open Platform. It performs no retries.
func (c *OpenUploadClient) Post(ctx context.Context, uploadToken string, metadata ArchiveMetadata) (string, error) {
	if c == nil || !safeUploadToken(uploadToken) {
		return "", ErrOpenUploadInvalid
	}
	if err := metadata.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Title       string `json:"title"`
		Description string `json:"desc"`
		CategoryID  int    `json:"tid"`
		Tags        string `json:"tag"`
		Copyright   int    `json:"copyright"`
		NoReprint   int    `json:"no_reprint"`
		Source      string `json:"source,omitempty"`
	}{
		Title: metadata.Title, Description: metadata.Description, CategoryID: metadata.CategoryID,
		Tags: strings.Join(metadata.Tags, ","), Copyright: metadata.Copyright,
		NoReprint: boolToOpenAPIInt(metadata.NoReprint), Source: metadata.Source,
	})
	if err != nil {
		return "", ErrOpenUploadInvalid
	}
	query := make(url.Values)
	query.Set("upload_token", uploadToken)
	data, err := c.signedRequest(ctx, http.MethodPost, openArchivePostURL, query, body)
	if err != nil {
		return "", err
	}
	var result struct {
		ResourceID string `json:"resource_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !safeArchiveResourceID(result.ResourceID) {
		return "", ErrOpenUploadResponse
	}
	return result.ResourceID, nil
}

// View obtains an archive's currently reported state. Published is true only
// for the documented combination state=0 and state_desc="开放浏览"; every other
// successfully decoded state remains unconfirmed (false).
func (c *OpenUploadClient) View(ctx context.Context, resourceID string) (ArchiveStatus, error) {
	if c == nil || !safeArchiveResourceID(resourceID) {
		return ArchiveStatus{}, ErrOpenUploadInvalid
	}
	query := make(url.Values)
	query.Set("resource_id", resourceID)
	data, err := c.signedRequest(ctx, http.MethodGet, openArchiveViewURL, query, nil)
	if err != nil {
		return ArchiveStatus{}, err
	}
	var result struct {
		ResourceID string `json:"resource_id"`
		AdditInfo  *struct {
			State     *int   `json:"state"`
			StateDesc string `json:"state_desc"`
		} `json:"addit_info"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.ResourceID != resourceID || result.AdditInfo == nil || result.AdditInfo.State == nil {
		return ArchiveStatus{}, ErrOpenUploadResponse
	}
	return ArchiveStatus{Published: *result.AdditInfo.State == 0 && result.AdditInfo.StateDesc == "开放浏览"}, nil
}

func boolToOpenAPIInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func safeArchiveResourceID(value string) bool {
	// The documented archive examples use the canonical 12-byte BV form. Do
	// not accept arbitrary provider text here: this value is returned to the
	// caller and later placed in a signed query parameter.
	if len(value) != 12 || !strings.HasPrefix(value, "BV") {
		return false
	}
	for _, r := range value[2:] {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

func containsArchiveControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
