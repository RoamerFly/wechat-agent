package linuxnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Status struct {
	Authenticated     bool   `json:"authenticated"`
	Connection        string `json:"connection"`
	IncomingTransport string `json:"incoming_transport"`
	AccountID         string `json:"-"`
	StorageMode       string `json:"-"`
	LoginObserved     bool   `json:"-"`
}

func (s *Sender) Status(ctx context.Context) (Status, error) {
	var raw struct {
		Authenticated *bool  `json:"authenticated"`
		ID            string `json:"id"`
		StorageMode   string `json:"storageMode"`
		Connection    string `json:"connection"`
		Transport     string `json:"incomingTransport"`
	}
	if err := s.readJSON(ctx, s.accountPath(), &raw); err != nil {
		return Status{Connection: "unavailable"}, err
	}
	return Status{Authenticated: raw.Authenticated != nil && *raw.Authenticated, Connection: raw.Connection, IncomingTransport: raw.Transport,
		AccountID: raw.ID, StorageMode: raw.StorageMode, LoginObserved: raw.Authenticated != nil}, nil
}

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Sender) Groups(ctx context.Context, search string) ([]Group, error) {
	if len(search) > 256 {
		return nil, errors.New("group search too long")
	}
	type groupPage struct {
		Items []struct {
			ID     string `json:"id"`
			RoomID string `json:"roomId"`
			Name   string `json:"displayName"`
		} `json:"items"`
		NextCursor *string `json:"nextCursor"`
	}
	out := []Group{}
	cursor := ""
	seen := map[string]bool{}
	ids := map[string]bool{}
	for count := 0; count < 20; count++ {
		var page groupPage
		path := s.accountPath() + "/groups?limit=100&search=" + url.QueryEscape(search)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := s.readJSON(ctx, path, &page); err != nil {
			return nil, err
		}
		if len(page.Items) > 100 {
			return nil, errors.New("invalid group page")
		}
		for _, g := range page.Items {
			if g.RoomID != "" {
				g.ID = g.RoomID
			}
			if !ids[g.ID] {
				out = append(out, Group{g.ID, g.Name})
				ids[g.ID] = true
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return out, nil
		}
		cursor = *page.NextCursor
		if len(cursor) > 8192 || seen[cursor] {
			return nil, errors.New("invalid group cursor")
		}
		seen[cursor] = true
	}
	return nil, errors.New("group pagination limit; narrow the search")
}

type LoginOperation struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	QRAvailable  bool   `json:"qr_available"`
	QRGeneration int    `json:"qr_generation"`
	Code         string `json:"code"`
}

var ErrLoginOutcomeUnknown = errors.New("login submission outcome unknown")

type nativeOperation struct {
	AccountID string `json:"accountId"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	State     string `json:"state"`
	Result    struct {
		QRAvailable  bool            `json:"qrAvailable"`
		QRGeneration int             `json:"qrGeneration"`
		Code         json.RawMessage `json:"code"`
	} `json:"result"`
}

func operationView(op nativeOperation) LoginOperation {
	code := ""
	if len(op.Result.Code) > 0 && len(op.Result.Code) < 128 {
		var value string
		if json.Unmarshal(op.Result.Code, &value) == nil {
			code = value
		} else {
			var n int64
			if json.Unmarshal(op.Result.Code, &n) == nil {
				code = strconv.FormatInt(n, 10)
			}
		}
	}
	return LoginOperation{op.ID, op.State, op.Result.QRAvailable, op.Result.QRGeneration, code}
}

func (s *Sender) BeginLogin(ctx context.Context, key string) (LoginOperation, error) {
	// Create only if this exact account is absent. No arbitrary account/profile
	// mutation endpoints are exposed through the web dashboard.
	response, err := s.do(ctx, http.MethodGet, s.accountPath(), nil, "")
	if err != nil {
		return LoginOperation{}, err
	}
	status := response.StatusCode
	response.Body.Close()
	if status == http.StatusNotFound {
		body, _ := json.Marshal(map[string]string{"id": s.account, "alias": s.account})
		response, err = s.do(ctx, http.MethodPost, "/v1/accounts", body, "")
		if err != nil {
			return LoginOperation{}, err
		}
		response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 201 {
			return LoginOperation{}, errors.New("native account creation rejected")
		}
	} else if status != 200 {
		return LoginOperation{}, errors.New("native account unavailable")
	}
	return s.submitQR(ctx, key, "WechatBot", false)
}

func (s *Sender) submitQR(ctx context.Context, key, computerName string, verifyIdentity bool) (LoginOperation, error) {
	body, _ := json.Marshal(map[string]any{"strategy": "qr", "forceQr": true, "allowQr": true, "timeoutSeconds": 300, "computerName": computerName, "qrRefresh": map[string]any{"mode": "manual", "maxRefreshes": 0}})
	response, err := s.do(ctx, http.MethodPost, s.accountPath()+"/auth/login", body, key)
	if err != nil {
		return LoginOperation{}, ErrLoginOutcomeUnknown
	}
	defer response.Body.Close()
	var op nativeOperation
	if verifyIdentity && response.StatusCode == http.StatusConflict {
		return LoginOperation{}, ErrAccountLoginBusy
	}
	if response.StatusCode != 202 {
		return LoginOperation{}, errors.New("login operation unavailable")
	}
	if !decodeBounded(response.Body, &op) || !safeID(op.ID) || (verifyIdentity && (op.AccountID != s.account || op.Kind != "login")) {
		return LoginOperation{}, ErrLoginOutcomeUnknown
	}
	return operationView(op), nil
}

func (s *Sender) LoginStatus(ctx context.Context, id string) (LoginOperation, error) {
	if !safeID(id) {
		return LoginOperation{}, errors.New("invalid login operation")
	}
	var op nativeOperation
	if err := s.readJSON(ctx, s.accountPath()+"/operations/"+id, &op); err != nil {
		return LoginOperation{}, err
	}
	if op.ID != id {
		return LoginOperation{}, errors.New("login operation mismatch")
	}
	return operationView(op), nil
}

func (s *Sender) LoginQR(ctx context.Context, id string, generation int) ([]byte, error) {
	if !safeID(id) || generation < 0 || generation > 100 {
		return nil, errors.New("invalid QR operation")
	}
	response, err := s.do(ctx, http.MethodGet, s.accountPath()+"/operations/"+id+"/qr?generation="+strconv.Itoa(generation), nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("QR unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errors.New("invalid QR image")
	}
	return data, nil
}

func (s *Sender) do(ctx context.Context, method, path string, body []byte, key string) (*http.Response, error) {
	// Caller owns and closes response.Body; this request context stays alive
	// until the response body has been consumed or the parent's deadline ends.
	u := s.base
	relative, err := url.Parse(path)
	if err != nil {
		return nil, errors.New("invalid native path")
	}
	u.Path, u.RawPath, u.RawQuery = relative.Path, relative.RawPath, relative.RawQuery
	request, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid native request")
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	request.GetBody = nil
	return s.client.Do(request)
}

// NewAdmin returns a client with the same loopback-only restrictions and an
// overall request deadline for account operations/QR bodies.
func NewAdmin(config Config) (*Sender, error) {
	s, err := New(config)
	if err != nil {
		return nil, err
	}
	s.client.Timeout = 30 * time.Second
	return s, nil
}

// EndAccountSession addresses only this profile. A never-enrolled profile
// returns 404 on the profile lookup, with no login operation to cancel.
func (s *Sender) EndAccountSession(ctx context.Context, forget bool, primary string) error {
	response, err := s.do(ctx, http.MethodGet, s.accountPath(), nil, "")
	if err != nil {
		return err
	}
	var profile struct{ ID, Alias string }
	verified := response.StatusCode == 200 && decodeBounded(response.Body, &profile)
	response.Body.Close()
	if response.StatusCode == 404 {
		return nil
	}
	if !verified || profile.ID != s.account || strings.EqualFold(profile.Alias, primary) || strings.EqualFold(profile.ID, primary) {
		return errors.New("无法核对微信账号状态")
	}
	actions := []string{"logout"}
	if forget {
		actions = append(actions, "forget-credentials")
	}
	for _, action := range actions {
		var body []byte
		if action == "forget-credentials" {
			body = []byte(`{"confirm":true}`)
		}
		response, err = s.do(ctx, http.MethodPost, s.accountPath()+"/"+action, body, randomKey())
		if err != nil {
			return err
		}
		var op nativeOperation
		valid := response.StatusCode == 202 && decodeBounded(response.Body, &op) && safeID(op.ID)
		response.Body.Close()
		if !valid {
			return errors.New("原生服务未接受退出或清理登录资料的请求")
		}
		state := op.State
		for state == "queued" || state == "submitting" || state == "running" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			status, err := s.LoginStatus(ctx, op.ID)
			if err != nil {
				return err
			}
			state = status.State
		}
		if state != "succeeded" {
			return errors.New("原生服务未确认操作完成")
		}
	}
	return nil
}
