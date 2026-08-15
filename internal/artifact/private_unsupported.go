//go:build !darwin && !linux && !windows

package artifact

import (
	"errors"
	"os"
)

func openPrivateArtifactRoot(string, string, bool) (privateArtifactRoot, string, error) {
	return nil, "", errors.New("artifact private storage is unsupported")
}

func ensurePrivateArtifactRoot(string) error {
	return errors.New("artifact private storage is unsupported")
}

func createPrivateArtifactFile(string) (*os.File, error) {
	return nil, errors.New("artifact private storage is unsupported")
}

func openPrivateArtifactFile(string) (*os.File, error) {
	return nil, errors.New("artifact private storage is unsupported")
}
