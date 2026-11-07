package apphost

import "context"

// ConnectionRef keeps a platform identifier scoped to a host registration.
type ConnectionRef struct {
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	ConnectionID   string `json:"connection_id"`
	AccountID      string `json:"account_id"`
}

type EventRef struct {
	EventID   string `json:"event_id"`
	MessageID string `json:"message_id"`
	ServerID  string `json:"server_id,omitempty"`
	SenderID  string `json:"sender_id,omitempty"`
	Revision  int    `json:"revision"`
}

type Event struct {
	ConnectionRef
	EventRef
	ConversationID string     `json:"conversation_id"`
	DatasetID      string     `json:"dataset_id"`
	MessageType    int        `json:"message_type,omitempty"`
	OccurredAt     string     `json:"occurred_at,omitempty"`
	ReceivedAt     string     `json:"received_at,omitempty"`
	StoredSequence int64      `json:"stored_sequence"`
	SourceSequence int64      `json:"source_sequence,omitempty"`
	AddresseeIDs   []string   `json:"addressee_ids,omitempty"`
	Quote          *EventRef  `json:"quote,omitempty"`
	Media          []MediaRef `json:"media,omitempty"`
	ReadScope      string     `json:"read_scope"`
}

type MediaRef struct {
	AssetID string `json:"asset_id,omitempty"`
	Kind    string `json:"kind"`
}

type Receipt struct {
	ConnectionRef
	DeliveryReceipt
	ConversationID string               `json:"conversation_id"`
	Deliveries     []DeliveryReceipt    `json:"deliveries,omitempty"`
	SupersededBy   string               `json:"superseded_by,omitempty"`
	ResumedFrom    string               `json:"resumed_from,omitempty"`
	Continuation   *ReceiptContinuation `json:"continuation,omitempty"`
}

type ReceiptContinuation struct {
	State       string `json:"state"`
	Message     string `json:"message"`
	Code        string `json:"code,omitempty"`
	DeliveryKey string `json:"delivery_key,omitempty"`
}

type DeliveryReceipt struct {
	IdempotencyKey   string        `json:"idempotency_key,omitempty"`
	OperationID      string        `json:"operation_id,omitempty"`
	Kind             string        `json:"kind"`
	State            string        `json:"state"`
	Lookup           string        `json:"lookup"`
	PlatformState    string        `json:"platform_state,omitempty"`
	PlatformCode     string        `json:"platform_code,omitempty"`
	ServerID         string        `json:"server_id,omitempty"`
	AcceptedParts    []ReceiptPart `json:"accepted_parts,omitempty"`
	UnconfirmedParts []ReceiptPart `json:"unconfirmed_parts,omitempty"`
	PartsIncomplete  bool          `json:"parts_incomplete,omitempty"`
	CancelRequested  bool          `json:"cancel_requested,omitempty"`
}

type ReceiptPart struct {
	Index    int    `json:"index"`
	ServerID string `json:"server_id,omitempty"`
	State    string `json:"state,omitempty"`
}

// EventReader exposes existing sources through an exact app connection.
type EventReader interface {
	ReadEvent(ctx context.Context, connectionID, conversationID, messageID string) (Event, error)
}

// ReceiptReader may query a previously submitted operation, without resubmitting it.
type ReceiptReader interface {
	ReadReceipt(ctx context.Context, connectionID, key string) (Receipt, error)
}

// ReceiptLister reads local delivery checkpoints; it never submits or retries.
type ReceiptLister interface {
	ListReceipts(ctx context.Context, connectionID, after string, limit int) (ReceiptPage, error)
}

type ReceiptPage struct {
	Items     []Receipt `json:"items"`
	NextAfter string    `json:"next_after,omitempty"`
	Snapshot  string    `json:"snapshot,omitempty"`
	Coverage  string    `json:"coverage"`
}

// ActionIntent is issued by the host at an existing authorization boundary.
// It is persisted with the claim for audit, and does not authorize replay.
type ActionIntent struct {
	ConnectionRef
	Action          string `json:"action"`
	RequesterID     string `json:"requester_id"`
	Source          string `json:"source"`
	SourceThread    string `json:"source_thread,omitempty"`
	SourceRequestID string `json:"source_request_id"`
	Target          string `json:"target,omitempty"`
	Purpose         string `json:"purpose"`
	GrantVersion    string `json:"grant_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type ActionReceipt struct {
	ConnectionRef
	OperationID         string `json:"operation_id"`
	Action              string `json:"action"`
	Target              string `json:"target,omitempty"`
	State               string `json:"state"`
	Lookup              string `json:"lookup"`
	PlatformOperationID string `json:"platform_operation_id,omitempty"`
	PlatformState       string `json:"platform_state,omitempty"`
	PlatformCode        string `json:"platform_code,omitempty"`
}

type ActionReader interface {
	ReadAction(ctx context.Context, connectionID, operationID string) (ActionReceipt, error)
}
