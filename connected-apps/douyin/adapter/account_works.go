package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"wechat-bot/internal/apphost"
)

const (
	accountWorksContentID    = "account-works"
	accountWorksDefaultSize  = 10
	accountWorksMaximumPages = 4
)

func (p *SearchApp) currentAccount() (accountAuthorization, error) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	return p.readAccount()
}

func accountWorksAccountID(account accountAuthorization) string {
	if account.OpenID == "" {
		return "official-authorized-account"
	}
	digest := sha256.Sum256([]byte("douyin-account-works-v1\x00" + account.ClientKey + "\x00" + account.OpenID))
	return "authorized-" + hex.EncodeToString(digest[:16])
}

func (p *SearchApp) worksAccount() (accountAuthorization, error) {
	account, err := p.currentAccount()
	if err != nil {
		return accountAuthorization{}, err
	}
	if !account.ReadWorks || account.AccessToken == "" {
		return accountAuthorization{}, apphost.ErrObservationForbidden
	}
	if !p.nowUTC().Before(account.ExpiresAt) {
		return accountAuthorization{}, apphost.ErrObservationCredentialExpired
	}
	return account, nil
}

func (p *SearchApp) accountWorksTargets() ([]apphost.ObservationTarget, error) {
	account, err := p.worksAccount()
	if err != nil {
		return nil, nil
	}
	return []apphost.ObservationTarget{{
		ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: accountConnectionID, AccountID: accountWorksAccountID(account)},
		ContentID:     accountWorksContentID,
		Label:         "授权账号的作品列表",
		Fields:        []apphost.ObservationField{apphost.ObservationAccountWorks},
	}}, nil
}

func (p *SearchApp) accountWorksGrant(access apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	account, err := p.worksAccount()
	if err != nil {
		return apphost.ObservationGrant{}, err
	}
	if !accountWorksMatches(account, access) {
		return apphost.ObservationGrant{}, apphost.ErrObservationForbidden
	}
	return apphost.ObservationGrant{Allowed: true, Version: account.grantRevision()}, nil
}

func accountWorksMatches(account accountAuthorization, access apphost.ObservationAccess) bool {
	return access.AppID == ID && access.InstallationID == installationID && access.ConnectionID == accountConnectionID &&
		access.AccountID == accountWorksAccountID(account) && access.ContentID == accountWorksContentID && access.Field == apphost.ObservationAccountWorks
}

type accountWorksCursor struct {
	Revision string  `json:"revision"`
	Count    int     `json:"count"`
	Position int     `json:"position"`
	Previous []int64 `json:"previous"`
}

func parseAccountWorksCursor(request apphost.ObservationRequest, revision string) (accountWorksCursor, error) {
	count := request.Limit
	if count < 0 || count > maxAccountWorksPageSize {
		return accountWorksCursor{}, apphost.ErrObservationAccountWorksPageSize
	}
	if request.Cursor == "" {
		if count == 0 {
			count = accountWorksDefaultSize
		}
		return accountWorksCursor{Revision: revision, Count: count}, nil
	}
	if len(request.Cursor) > 512 {
		return accountWorksCursor{}, apphost.ErrObservationInvalid
	}
	var cursor accountWorksCursor
	raw, err := base64.RawURLEncoding.DecodeString(request.Cursor)
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Revision != revision ||
		cursor.Count <= 0 || cursor.Count > maxAccountWorksPageSize || cursor.Position < 0 || cursor.Position > math.MaxInt-cursor.Count ||
		len(cursor.Previous) == 0 || len(cursor.Previous) >= accountWorksMaximumPages {
		return accountWorksCursor{}, apphost.ErrObservationInvalid
	}
	if count != 0 && cursor.Count != count {
		return accountWorksCursor{}, apphost.ErrObservationAccountWorksPageSize
	}
	return cursor, nil
}

func (p *SearchApp) readAccountWorks(ctx context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	account, err := p.worksAccount()
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	if !accountWorksMatches(account, request.ObservationAccess) {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	cursor, err := parseAccountWorksCursor(request, account.grantRevision())
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	var position int64
	if n := len(cursor.Previous); n > 0 {
		position = cursor.Previous[n-1]
	}
	response, err := p.fetchAccountWorks(ctx, account, position, cursor.Count)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	grant, err := p.accountWorksGrant(request.ObservationAccess)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	if !grant.Allowed || grant.Version != account.grantRevision() {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	return accountWorksPage(response, cursor, accountWorksAccountID(account))
}

func accountWorksPage(response accountWorksWirePage, cursor accountWorksCursor, accountID string) (apphost.ObservationPage, error) {
	coverage := fmt.Sprintf("抖音账号作品列表第 %d 页 · 请求每页 %d 条，返回 %d 条 · 可见性待核对", len(cursor.Previous)+1, cursor.Count, len(response.Items))
	page := apphost.ObservationPage{Coverage: coverage, Complete: !response.HasMore}
	for _, item := range response.Items {
		sourceID, isVideoID := item.identifier()
		if !isVideoID {
			digest := sha256.Sum256([]byte(accountID + "\x00" + sourceID))
			sourceID = "account-work-" + hex.EncodeToString(digest[:16])
		}
		text := "账号作品，公开状态待核对"
		if item.CreateTime > 0 {
			text += "。平台记录创建时间 " + time.Unix(item.CreateTime, 0).UTC().Format(time.RFC3339)
		}
		page.Chunks = append(page.Chunks, apphost.ObservationChunk{Position: cursor.Position, Text: text, SourceID: sourceID})
		cursor.Position++
	}
	if page.Complete {
		return page, nil
	}
	if slices.Contains(cursor.Previous, response.Cursor) {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	if len(cursor.Previous)+1 >= accountWorksMaximumPages {
		page.Coverage += "。已达单轮四页上限，平台仍提示有更多记录"
		page.Complete = true
		return page, nil
	}
	cursor.Previous = append(cursor.Previous, response.Cursor)
	raw, err := json.Marshal(cursor)
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	return page, nil
}
