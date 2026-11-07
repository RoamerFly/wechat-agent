package adapter

import (
	"context"
	"encoding/json"
	"net/http"
)

// AccountOpenID obtains the opaque Open Platform identity for the authorized
// account. It intentionally returns no profile data such as a display name or
// avatar, and it does not surface provider response text in errors.
func (c *OpenUploadClient) AccountOpenID(ctx context.Context) (string, error) {
	if c == nil {
		return "", ErrOpenUploadInvalid
	}
	data, err := c.signedRequest(ctx, http.MethodGet, openAccountInfoURL, nil, nil)
	if err != nil {
		return "", err
	}
	var result struct {
		OpenID string `json:"openid"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !safeOpenID(result.OpenID) {
		return "", ErrOpenUploadResponse
	}
	return result.OpenID, nil
}

func safeOpenID(value string) bool {
	// OpenIDs are opaque provider identifiers. Accept printable ASCII only so
	// a returned value cannot introduce whitespace, control characters, or
	// ambiguous Unicode into later identity matching or logs.
	return safeHeaderValue(value, 4096)
}
