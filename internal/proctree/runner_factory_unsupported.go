//go:build !darwin && !linux && !windows

package proctree

func NewRunner(options Options) (Runner, error) {
	return newUnsupportedRunner(options)
}
