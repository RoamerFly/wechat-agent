package station

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"wechat-bot/internal/identity"
	"wechat-bot/internal/workspacefs"
)

const stationControlJournalBytes = 2 << 20

// Callers hold deliveryMu. These journals contain host controls, not chat data.
func readStationControlJournal[T any](dir, name string) (map[string]T, error) {
	rows := map[string]T{}
	root, err := workspacefs.OpenPath(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return rows, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > stationControlJournalBytes {
		return nil, errors.New("control journal unavailable")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, stationControlJournalBytes+1))
	if err != nil || len(raw) > stationControlJournalBytes || json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 5000 {
		return nil, errors.New("invalid control journal")
	}
	return rows, nil
}

func writeStationControlJournal[T any](dir, name string, rows map[string]T) error {
	raw, err := json.Marshal(rows)
	if err != nil || len(raw) > stationControlJournalBytes || len(rows) > 5000 {
		return errors.New("control journal full")
	}
	root, err := workspacefs.OpenPath(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	stage := ".control-" + identity.RandomToken()
	defer root.Remove(stage)
	if err := writeLegacyStagedFile(root, stage, rows); err != nil {
		return err
	}
	return root.Rename(stage, name)
}
