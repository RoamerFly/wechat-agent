package linuxnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"wechat-bot/internal/sending"
)

var ErrToolNotFound = errors.New("native tool resource not found")

// Fixed admission failures are safe to report; never expose arbitrary native
// HTTP bodies, XML, account paths or credential-bearing upstream diagnostics.
type ToolRejection struct{ Code string }

func (e *ToolRejection) Error() string { return "native tool request rejected" }
func QuoteAdmissionReason(err error) string {
	var rejection *ToolRejection
	if errors.As(err, &rejection) {
		return rejection.Code
	}
	return ""
}

func safeThreadID(value string) bool {
	if strings.HasSuffix(value, "@chatroom") {
		return safeID(strings.TrimSuffix(value, "@chatroom"))
	}
	return safeID(value)
}

type Capabilities struct {
	MediaKinds                []string         `json:"mediaKinds"`
	MediaSendMaxBytes         map[string]int64 `json:"mediaSendMaxBytes"`
	QuotedTextSend            bool             `json:"quotedTextSend"`
	GroupMutations            []string         `json:"groupMutations"`
	ThreadNotificationTargets []string         `json:"threadNotificationTargets"`
}
type UploadSource struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ByteLength  int64  `json:"byte_length"`
	ContentType string `json:"content_type"`
}
type ToolSend struct {
	Title          string     `json:"title,omitempty"`
	URL            string     `json:"url,omitempty"`
	Description    string     `json:"description,omitempty"`
	ThreadID       string     `json:"thread_id"`
	Kind           string     `json:"kind"`
	Text           string     `json:"text"`
	SourceID       string     `json:"source_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	Quote          *ToolQuote `json:"quote,omitempty"`
}
type ToolQuote struct {
	MessageID string `json:"message_id"`
	ServerID  string `json:"server_id"`
}
type ToolStatus struct {
	AccountID            string             `json:"-"`
	CreatedAt            string             `json:"created_at,omitempty"`
	SourceID             string             `json:"source_id,omitempty"`
	Attempts             int                `json:"attempts,omitempty"`
	Retryable            bool               `json:"retryable,omitempty"`
	CancelRequested      bool               `json:"cancel_requested,omitempty"`
	ID                   string             `json:"id"`
	State                string             `json:"state"`
	Phase                string             `json:"phase"`
	Kind                 string             `json:"kind"`
	ThreadID             string             `json:"thread_id"`
	ServerID             string             `json:"server_id"`
	Reason               string             `json:"reason"`
	ErrorCode            string             `json:"error_code,omitempty"`
	VoicePartsIncomplete bool               `json:"voice_parts_incomplete,omitempty"`
	VoiceChunks          []VoiceChunkStatus `json:"voice_chunks,omitempty"`
	VoiceFailureStage    string             `json:"voice_failure_stage,omitempty"`
	VoiceTransportKind   string             `json:"voice_transport_kind,omitempty"`
	VoiceFailedOffset    *int64             `json:"voice_failed_offset,omitempty"`
}

type VoiceChunkStatus struct {
	ClientID      string `json:"client_id,omitempty"`
	EncodedMD5    string `json:"encoded_md5,omitempty"`
	ServerID      string `json:"server_id,omitempty"`
	Index         int    `json:"index"`
	State         string `json:"state"`
	DurationMS    int64  `json:"duration_ms"`
	PacingMS      int64  `json:"pacing_ms"`
	SubmittedAtMS int64  `json:"submitted_at_ms,omitempty"`
	AcceptedAtMS  int64  `json:"accepted_at_ms,omitempty"`
	HasReceipt    bool   `json:"has_receipt"`
	PrefixRetries int    `json:"prefix_retries,omitempty"`
}

func (s *Sender) toolJSON(ctx context.Context, method, path, key string, body io.Reader, contentType string) ([]byte, error) {
	u := s.base
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || relative.Host != "" || !strings.HasPrefix(relative.Path, s.accountPath()+"/") {
		return nil, errors.New("invalid tool path")
	}
	u.Path, u.RawPath, u.RawQuery = relative.Path, relative.RawPath, relative.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("invalid tool request")
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.GetBody = nil
	response, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrWriteNotAdmitted) {
			return nil, ErrWriteNotAdmitted
		}
		return nil, errors.New("native tool outcome unavailable; do not resend automatically")
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return nil, ErrToolNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == 400 {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &failure) == nil {
				switch failure.Error.Code {
				case "unsupported_quote_type", "invalid_quote_reference", "quote_target_mismatch", "quote_target_unavailable", "quote_content_unavailable", "quote_sender_unavailable", "quote_target_ambiguous":
					return nil, &ToolRejection{Code: failure.Error.Code}
				case "channels_share_identity_missing", "channels_share_identity_mismatch", "channels_share_lookup_rejected", "channels_share_response_invalid", "channels_share_link_invalid", "channels_object_id_missing", "channels_received_card_required":
					return nil, &ToolRejection{Code: failure.Error.Code}
				}
			}
		}
		return nil, errors.New("native tool request rejected")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || !json.Valid(data) {
		return nil, errors.New("invalid native tool response")
	}
	return data, nil
}
func (s *Sender) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	raw, err := s.toolJSON(ctx, "GET", s.accountPath()+"/capabilities", "", nil, "")
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}
func (s *Sender) Upload(ctx context.Context, filename, contentType string, file io.Reader, size int64) (UploadSource, error) {
	return s.upload(ctx, filename, contentType, file, size, "")
}

// UploadVerified binds the staged native source to the already verified bytes.
func (s *Sender) UploadVerified(ctx context.Context, filename, contentType string, file io.Reader, size int64, sha256 string) (UploadSource, error) {
	if len(sha256) != 64 {
		return UploadSource{}, errors.New("invalid upload digest")
	}
	return s.upload(ctx, filename, contentType, file, size, sha256)
}

func (s *Sender) upload(ctx context.Context, filename, contentType string, file io.Reader, size int64, sha256 string) (UploadSource, error) {
	out := UploadSource{Filename: filename, ContentType: contentType, ByteLength: size}
	if filename == "" || len(filename) > 256 || strings.ContainsAny(filename, "/\\\r\n\x00") || size <= 0 || size > 512<<20 {
		return out, errors.New("invalid upload")
	}
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	formType := form.FormDataContentType()
	done := make(chan error, 1)
	go func() {
		part, err := form.CreateFormFile("file", filename)
		if err == nil {
			var count int64
			count, err = io.Copy(part, io.LimitReader(file, size+1))
			if err == nil && count != size {
				err = errors.New("upload size changed")
			}
		}
		if err == nil {
			err = form.Close()
		}
		writer.CloseWithError(err)
		done <- err
	}()
	raw, err := s.toolJSON(ctx, "POST", s.accountPath()+"/uploads", "", reader, formType)
	reader.Close()
	writeErr := <-done
	if err != nil {
		return out, err
	}
	if writeErr != nil {
		return out, errors.New("upload stream failed")
	}
	var source struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Bytes  int64  `json:"byteLength"`
		SHA256 string `json:"sha256"`
	}
	if json.Unmarshal(raw, &source) != nil || !safeID(source.ID) {
		return out, errors.New("invalid upload identity")
	}
	if sha256 != "" && (source.Name != filename || source.Bytes != size || source.SHA256 != sha256) {
		return out, errors.New("native_upload_integrity_mismatch")
	}
	out.ID = source.ID
	return out, nil
}
func (s *Sender) Submit(ctx context.Context, send ToolSend) (ToolStatus, error) {
	if !safeThreadID(send.ThreadID) || !safeID(send.IdempotencyKey) || len(send.IdempotencyKey) > 128 {
		return ToolStatus{}, errors.New("invalid send identity")
	}
	input := map[string]any{"threadId": send.ThreadID, "kind": send.Kind}
	if send.Kind == "text" {
		if strings.TrimSpace(send.Text) == "" || len(send.Text) > 4096 || !utf8.ValidString(send.Text) {
			return ToolStatus{}, errors.New("invalid text")
		}
		input["text"] = send.Text
		if send.Quote != nil {
			if send.Quote.MessageID == "" || len(send.Quote.MessageID) > 8192 || send.Quote.ServerID == "" || len(send.Quote.ServerID) > 20 {
				return ToolStatus{}, errors.New("invalid quote")
			}
			input["quote"] = map[string]string{"messageId": send.Quote.MessageID, "serverId": send.Quote.ServerID}
		}
	} else if send.Kind == "share" {
		u, e := url.Parse(send.URL)
		if e != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || len(send.URL) > 2048 || strings.TrimSpace(send.Title) == "" || len(send.Title) > 1024 || len(send.Description) > 2048 || send.Quote != nil {
			return ToolStatus{}, errors.New("invalid share send")
		}
		input["title"] = send.Title
		input["url"] = send.URL
		if send.Description != "" {
			input["description"] = send.Description
		}
	} else {
		if (send.Kind != "image" && send.Kind != "file" && send.Kind != "video" && send.Kind != "voice") || !safeID(send.SourceID) || send.Quote != nil {
			return ToolStatus{}, errors.New("invalid media send")
		}
		input["sourceId"] = send.SourceID
	}
	body, _ := json.Marshal(input)
	raw, err := s.toolJSON(ctx, "POST", s.sendsPath(), send.IdempotencyKey, bytes.NewReader(body), "application/json")
	if err != nil {
		return ToolStatus{}, err
	}
	status, err := s.decodeToolStatus(raw)
	if err == nil && status.ThreadID != send.ThreadID {
		return ToolStatus{}, errors.New("send destination mismatch")
	}
	status.Kind = send.Kind
	return status, err
}
func (s *Sender) decodeToolStatus(raw []byte) (ToolStatus, error) {
	var value struct {
		CreatedAt       string `json:"createdAt"`
		SourceID        string `json:"sourceId"`
		Attempts        int    `json:"attempts"`
		Retryable       bool   `json:"retryable"`
		CancelRequested bool   `json:"cancelRequested"`
		ID              string `json:"id"`
		AccountID       string `json:"accountId"`
		State           string `json:"state"`
		Phase           string `json:"phase"`
		Kind            string `json:"kind"`
		Destination     string `json:"destination"`
		ThreadID        string `json:"threadId"`
		ServerID        string `json:"serverId"`
		ServerMessageID string `json:"serverMessageId"`
		Reason          string `json:"reason"`
		ErrorCode       string `json:"errorCode"`
		Result          struct {
			VoiceFailureStage  string `json:"voiceFailureStage"`
			VoiceTransportKind string `json:"voiceTransportKind"`
			VoiceFailedOffset  *int64 `json:"voiceFailedOffset"`
			Chunks             []struct {
				ClientID        string `json:"clientId"`
				EncodedMD5      string `json:"encodedMd5"`
				Index           int    `json:"index"`
				State           string `json:"state"`
				DurationMS      int64  `json:"durationMs"`
				PacingMS        int64  `json:"pacingMs"`
				SubmittedAtMS   int64  `json:"submittedAtMs"`
				AcceptedAtMS    int64  `json:"acceptedAtMs"`
				ServerMessageID string `json:"serverMessageId"`
				PrefixRetries   int    `json:"prefixRetries"`
			} `json:"chunks"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &value) != nil || !safeID(value.ID) || (value.AccountID != "" && value.AccountID != s.account) {
		return ToolStatus{}, errors.New("invalid send status")
	}
	if value.ThreadID == "" {
		value.ThreadID = value.Destination
	}
	if value.ServerID == "" {
		value.ServerID = value.ServerMessageID
	}
	if value.Reason == "" {
		value.Reason = value.ErrorCode
	}
	if value.State == "accepted" {
		id, err := strconv.ParseUint(value.ServerID, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != value.ServerID {
			return ToolStatus{}, errors.New("accepted send receipt missing exact remote identity")
		}
	}
	if len(value.Reason) > 128 {
		value.Reason = "native_job_failed"
	}
	status := ToolStatus{ID: value.ID, AccountID: value.AccountID, State: value.State, Phase: value.Phase, Kind: value.Kind, ThreadID: value.ThreadID, ServerID: value.ServerID, Reason: value.Reason}
	if len(value.ErrorCode) <= 128 {
		status.ErrorCode = value.ErrorCode
	}
	status.CreatedAt = value.CreatedAt
	if value.SourceID != "" && !safeID(value.SourceID) {
		return ToolStatus{}, errors.New("invalid media source identity")
	}
	status.SourceID = value.SourceID
	status.Attempts = value.Attempts
	status.Retryable = value.Retryable
	status.CancelRequested = value.CancelRequested
	for _, chunk := range value.Result.Chunks {
		if len(status.VoiceChunks) == 8 {
			break
		}
		if chunk.Index < 0 || chunk.Index > 7 || chunk.DurationMS < 1 || chunk.DurationMS > 60000 || chunk.PacingMS < 0 || chunk.PacingMS > 62000 {
			continue
		}
		switch chunk.State {
		case "prepared", "submitting", "accepted", "rejected", "needs_review", "cancelled":
		default:
			continue
		}
		server, err := strconv.ParseUint(chunk.ServerMessageID, 10, 64)
		hasReceipt := err == nil && server > 0 && strconv.FormatUint(server, 10) == chunk.ServerMessageID
		retries := chunk.PrefixRetries
		if retries < 0 || retries > 24 {
			retries = 0
		}
		serverID := ""
		if hasReceipt {
			serverID = chunk.ServerMessageID
		}
		status.VoiceChunks = append(status.VoiceChunks, VoiceChunkStatus{ClientID: chunk.ClientID, EncodedMD5: chunk.EncodedMD5, ServerID: serverID, Index: chunk.Index, State: chunk.State, DurationMS: chunk.DurationMS, PacingMS: chunk.PacingMS, SubmittedAtMS: chunk.SubmittedAtMS, AcceptedAtMS: chunk.AcceptedAtMS, HasReceipt: hasReceipt, PrefixRetries: retries})
	}
	status.VoicePartsIncomplete = len(status.VoiceChunks) != len(value.Result.Chunks)
	switch value.Result.VoiceFailureStage {
	case "voice_handshake_before_submission", "voice_upload_exchange_outcome_unknown", "voice_upload_receipt_unverified":
		status.VoiceFailureStage = value.Result.VoiceFailureStage
	}
	switch value.Result.VoiceTransportKind {
	case "voice_transport_timeout", "voice_transport_connect", "voice_transport_body", "voice_transport_closed", "voice_transport_io", "voice_transport_unclassified":
		status.VoiceTransportKind = value.Result.VoiceTransportKind
	}
	if offset := value.Result.VoiceFailedOffset; offset != nil && *offset >= 0 && *offset <= 186010 {
		status.VoiceFailedOffset = offset
	}
	return status, nil
}
func (s *Sender) ToolSendStatus(ctx context.Context, id string) (ToolStatus, error) {
	if !safeID(id) {
		return ToolStatus{}, errors.New("invalid send ID")
	}
	raw, err := s.toolJSON(ctx, "GET", s.sendsPath()+"/"+url.PathEscape(id), "", nil, "")
	if err != nil {
		return ToolStatus{}, err
	}
	return s.decodeToolStatus(raw)
}
func (s *Sender) LookupSend(ctx context.Context, key string) (ToolStatus, error) {
	if !safeID(key) || len(key) > 128 {
		return ToolStatus{}, errors.New("invalid idempotency key")
	}
	raw, err := s.toolJSON(ctx, "GET", s.sendsPath()+"/by-idempotency-key?key="+url.QueryEscape(key), "", nil, "")
	if err != nil {
		return ToolStatus{}, err
	}
	return s.decodeToolStatus(raw)
}

// SendImage submits one owned source and only polls the resulting job. A
// cancelled worker requests cancellation of its own pending job, never retry.
func (s *Sender) SendImage(ctx context.Context, recipient, source string) (ToolStatus, error) {
	return s.SendMedia(ctx, recipient, "image", source)
}
func (s *Sender) SendMedia(ctx context.Context, recipient, kind, source string) (ToolStatus, error) {
	status, err := s.Submit(ctx, ToolSend{ThreadID: recipient, Kind: kind, SourceID: source, IdempotencyKey: randomKey()})
	if err != nil {
		return status, sending.ErrOutcomeUnknown
	}
	id := status.ID
	defer func() {
		if ctx.Err() != nil && status.State != "accepted" {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = s.toolJSON(cleanup, "POST", s.accountPath()+"/jobs/"+url.PathEscape(id)+"/cancel", "", bytes.NewReader([]byte("{}")), "application/json")
		}
	}()
	for {
		switch status.State {
		case "accepted":
			return status, nil
		case "failed", "cancelled":
			return status, &sending.OutcomeError{Cause: sending.ErrRejected, Reason: safeSendReason(status.Reason), IntentID: id}
		case "needs_review":
			return status, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: safeSendReason(status.Reason), IntentID: id}
		}
		select {
		case <-ctx.Done():
			reason := "receipt_wait_timeout"
			if errors.Is(ctx.Err(), context.Canceled) {
				reason = "media_worker_cancelled"
			}
			return status, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: reason, IntentID: id}
		case <-time.After(time.Second):
		}
		status, err = s.ToolSendStatus(ctx, id)
		if err != nil {
			return status, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: "receipt_lookup_failed", IntentID: id}
		}
	}
}
