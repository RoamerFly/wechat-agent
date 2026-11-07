package linuxnative

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

var (
	ErrAccountLoginBusy    = errors.New("native login already in progress")
	ErrAccountLoginMissing = errors.New("native login operation not found")
)

// EnsureManagedAccount creates only the client's exact account. If a create
// response was lost, a repeat observes that same account ID before writing.
func (s *Sender) EnsureManagedAccount(ctx context.Context) (AccountInfo, error) {
	var account AccountInfo
	res, err := s.do(ctx, http.MethodGet, s.accountPath(), nil, "")
	if err != nil {
		return account, err
	}
	if res.StatusCode == http.StatusNotFound {
		res.Body.Close()
		body, _ := json.Marshal(map[string]string{"id": s.account})
		res, err = s.do(ctx, http.MethodPost, "/v1/accounts", body, "")
		if err != nil {
			return account, err
		}
	}
	defer res.Body.Close()
	if (res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated) ||
		!decodeBounded(res.Body, &account) || account.ID != s.account ||
		(account.Alias != "" && !safeID(account.Alias)) {
		return AccountInfo{}, errors.New("native account registration could not be verified")
	}
	return account, nil
}

// StartAccountQR uses the native durable request key. It requires an existing
// account and never creates or imports a profile as a side effect.
func (s *Sender) StartAccountQR(ctx context.Context, key string) (LoginOperation, error) {
	if !safeID(key) {
		return LoginOperation{}, errors.New("invalid login request key")
	}
	return s.submitQR(ctx, key, "Agent-home-station", true)
}

func (s *Sender) ReadAccountLogin(ctx context.Context, id string) (LoginOperation, error) {
	if !safeID(id) {
		return LoginOperation{}, ErrAccountLoginMissing
	}
	res, err := s.do(ctx, http.MethodGet, s.accountPath()+"/operations/"+id, nil, "")
	if err != nil {
		return LoginOperation{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return LoginOperation{}, ErrAccountLoginMissing
	}
	var op nativeOperation
	if res.StatusCode != http.StatusOK || !decodeBounded(res.Body, &op) {
		return LoginOperation{}, errors.New("native login state unavailable")
	}
	if op.ID != id || op.AccountID != s.account || op.Kind != "login" {
		return LoginOperation{}, ErrAccountLoginMissing
	}
	return operationView(op), nil
}
