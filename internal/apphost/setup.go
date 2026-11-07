package apphost

import "context"

// AccountSetup is an owner-only capability of a registered application.
type AccountSetup interface {
	ProvisionConnection(ctx context.Context, requestID string) (Connection, error)
	StartConnectionLogin(ctx context.Context, connectionID, requestID string) (LoginChallenge, error)
	ReadConnectionLogin(ctx context.Context, connectionID, operationID string) (LoginChallenge, error)
	ReadConnectionQR(ctx context.Context, connectionID, operationID string, generation int) ([]byte, error)
}

type LoginChallenge struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	QRAvailable  bool   `json:"qr_available"`
	QRGeneration int    `json:"qr_generation"`
}
