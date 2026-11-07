package linuxnative

import (
	"context"
	"errors"
)

// DaemonStatus is private ownership evidence for a native service.
type DaemonStatus struct {
	PID                 int    `json:"pid"`
	DataDirectory       string `json:"dataDir"`
	ServerID            string `json:"serverId"`
	OwnershipGeneration string `json:"ownershipGeneration"`
}

func (s *Sender) DaemonStatus(ctx context.Context) (DaemonStatus, error) {
	var status DaemonStatus
	err := s.readJSON(ctx, "/v1/status", &status)
	if err != nil || status.PID < 1 || status.DataDirectory == "" || !safeID(status.ServerID) || !safeID(status.OwnershipGeneration) {
		return DaemonStatus{}, errors.New("native daemon ownership is unavailable")
	}
	return status, nil
}
