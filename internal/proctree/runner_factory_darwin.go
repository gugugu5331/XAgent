//go:build darwin

package proctree

func NewRunner(options Options) (Runner, error) {
	return newDarwinRunner(options)
}
