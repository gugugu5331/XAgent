//go:build linux

package proctree

func NewRunner(options Options) (Runner, error) {
	return newLinuxRunner(options)
}
