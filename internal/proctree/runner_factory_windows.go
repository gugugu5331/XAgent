//go:build windows

package proctree

func NewRunner(options Options) (Runner, error) {
	return newWindowsRunner(options)
}
