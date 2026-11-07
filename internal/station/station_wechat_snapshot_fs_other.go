//go:build !windows

package station

import (
	"errors"
	"os"
)

var errLegacySnapshotPrivateFSUnsupported = errors.New("legacy snapshot private filesystem support requires Windows")

func createLegacySnapshotStage(parent *os.Root, name string) error {
	return errLegacySnapshotPrivateFSUnsupported
}

func verifyLegacySnapshotPrivacy(root *os.Root) error {
	return errLegacySnapshotPrivateFSUnsupported
}

func publishLegacySnapshot(parent *os.Root, temp, name string) error {
	return errLegacySnapshotPrivateFSUnsupported
}

func legacySnapshotLocalPath(root *os.Root) (string, error) {
	return "", errLegacySnapshotPrivateFSUnsupported
}
