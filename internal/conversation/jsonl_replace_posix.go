//go:build darwin || linux

package conversation

import "os"

func replaceV2FileAtomic(stage string, target string) error {
	return os.Rename(stage, target)
}
