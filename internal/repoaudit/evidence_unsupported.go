//go:build !unix && !windows

package repoaudit

import (
	"errors"
	"os"
)

var errEvidenceUnsupported = errors.New("evidence durability and lease are unsupported on this platform")

func createPrivateDirectory(string, os.FileMode) error { return errEvidenceUnsupported }
func createPrivateFile(string, os.FileMode, bool) (*os.File, error) {
	return nil, errEvidenceUnsupported
}
func lockEvidenceLease(*os.File) error                    { return errEvidenceUnsupported }
func unlockEvidenceLease(*os.File) error                  { return errEvidenceUnsupported }
func syncDirectory(string) error                          { return errEvidenceUnsupported }
func validatePrivateDirectory(string, os.FileMode) error  { return errEvidenceUnsupported }
func validatePrivateFile(string, os.FileMode, bool) error { return errEvidenceUnsupported }
