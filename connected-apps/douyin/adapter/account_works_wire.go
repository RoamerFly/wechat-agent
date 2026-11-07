package adapter

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"wechat-bot/internal/apphost"
)

// accountWorksEndpoint is the documented authorized-account list endpoint.
// It deliberately remains separate from public search and the posting-task
// status endpoint: the returned list is a creator-owned collection.
const accountWorksEndpoint = "https://open.douyin.com/video/list/"

const maxAccountWorksPageSize = math.MaxInt32

type accountWorksWirePage struct {
	Items   []accountWorkWireItem
	HasMore bool
	Cursor  int64
}

// Keep only source identifiers and creation time. This endpoint omits the
// current visibility state, so titles and share URLs are not retained.
type accountWorkWireItem struct {
	VideoID    json.RawMessage `json:"video_id"`
	ItemID     string          `json:"item_id"`
	CreateTime int64           `json:"create_time"`
}

type accountWorksWireResponse struct {
	Data *struct {
		ErrorCode *int                  `json:"error_code"`
		OpenID    *string               `json:"open_id"`
		HasMore   *bool                 `json:"has_more"`
		Cursor    *json.RawMessage      `json:"cursor"`
		List      []accountWorkWireItem `json:"list"`
	} `json:"data"`
	Extra *struct {
		ErrorCode *int `json:"error_code"`
	} `json:"extra"`
}

// fetchAccountWorks reads one bounded official list page. It has no fallback
// to web scraping: callers can reliably distinguish an expired grant, a
// missing scope, throttling and a transient provider outage.
func (p *SearchApp) fetchAccountWorks(ctx context.Context, account accountAuthorization, cursor int64, count int) (accountWorksWirePage, error) {
	if p == nil || p.client == nil || !openID.MatchString(account.OpenID) || !accessToken.MatchString(account.AccessToken) || cursor < 0 || count < 1 || count > maxAccountWorksPageSize {
		return accountWorksWirePage{}, apphost.ErrObservationInvalid
	}
	if !account.ReadWorks {
		return accountWorksWirePage{}, apphost.ErrObservationCapabilityDenied
	}
	if !p.nowUTC().Before(account.ExpiresAt) {
		return accountWorksWirePage{}, apphost.ErrObservationCredentialExpired
	}
	endpoint, err := url.Parse(accountWorksEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host != "open.douyin.com" {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	query := endpoint.Query()
	query.Set("open_id", account.OpenID)
	query.Set("cursor", strconv.FormatInt(cursor, 10))
	query.Set("count", strconv.Itoa(count))
	endpoint.RawQuery = query.Encode()

	readCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Agent-home-station/1.0 douyin-account-works")
	req.Header.Set("access-token", account.AccessToken)

	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return accountWorksWirePage{}, ctx.Err()
		}
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusPreconditionFailed {
		return accountWorksWirePage{}, apphost.ErrObservationThrottled
	}
	if response.StatusCode != http.StatusOK {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if len(body) > maxResponseBytes {
		return accountWorksWirePage{}, apphost.ErrObservationAccountWorksResponseSize
	}
	if err != nil {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	var decoded accountWorksWireResponse
	if json.Unmarshal(body, &decoded) != nil {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	// A nonzero code is useful even if a provider response omitted its other
	// envelope. Success always requires both explicit zero-valued envelopes.
	if decoded.Data != nil && decoded.Data.ErrorCode != nil && *decoded.Data.ErrorCode != 0 {
		return accountWorksWirePage{}, accountWorksResponseError(*decoded.Data.ErrorCode)
	}
	if decoded.Extra != nil && decoded.Extra.ErrorCode != nil && *decoded.Extra.ErrorCode != 0 {
		return accountWorksWirePage{}, accountWorksResponseError(*decoded.Extra.ErrorCode)
	}
	if decoded.Data == nil || decoded.Data.ErrorCode == nil || decoded.Extra == nil || decoded.Extra.ErrorCode == nil {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	if decoded.Data.OpenID != nil && *decoded.Data.OpenID != account.OpenID {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	if decoded.Data.HasMore == nil {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	result := accountWorksWirePage{Items: append([]accountWorkWireItem(nil), decoded.Data.List...), HasMore: *decoded.Data.HasMore}
	if decoded.Data.Cursor != nil {
		parsed, ok := parseAccountWorksWireCursor(*decoded.Data.Cursor)
		if !ok {
			return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
		}
		result.Cursor = parsed
	} else if result.HasMore {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	if len(result.Items) > count || (result.HasMore && (result.Cursor == 0 || result.Cursor == cursor)) {
		return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
	}
	seen := make(map[string]struct{}, len(result.Items))
	for _, item := range result.Items {
		id, isVideoID := item.identifier()
		if !isVideoID && !safeOpaque(id, 1, 512) || item.CreateTime < 0 || item.CreateTime > 253402300799 {
			return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
		}
		if _, duplicate := seen[id]; duplicate {
			return accountWorksWirePage{}, apphost.ErrObservationUpstreamUnavailable
		}
		seen[id] = struct{}{}
	}
	return result, nil
}

func parseAccountWorksWireCursor(raw json.RawMessage) (int64, bool) {
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		value, err := strconv.ParseInt(number.String(), 10, 64)
		return value, err == nil && value >= 0
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed >= 0
}

func (item accountWorkWireItem) identifier() (string, bool) {
	if id, ok := canonicalItemID(item.VideoID); ok {
		return id, true
	}
	return item.ItemID, false
}

func accountWorksResponseError(code int) error {
	switch code {
	case 0:
		return nil
	case 2190002, 2190008, 28001003, 28001008:
		return apphost.ErrObservationCredentialExpired
	case 28001014, 28001016, 28001018, 28001019:
		return apphost.ErrObservationCapabilityDenied
	case 28003017, 2118110:
		return apphost.ErrObservationThrottled
	case 2100004, 28001005, 28001006:
		return apphost.ErrObservationUpstreamUnavailable
	case 2100005:
		return apphost.ErrObservationInvalid
	default:
		return apphost.ErrObservationContentUnavailable
	}
}
