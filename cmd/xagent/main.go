package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr, runArgs))
}

func runArgs(args []string) error {
	if handled, err := runInternalMode(); handled {
		return err
	}
	runtime, err := buildAssemblyFromCLI(args)
	if err != nil {
		return err
	}
	if runtime == nil {
		return errors.New("运行时构造结果为空")
	}
	_, runErr := runtime.Run()
	closeErr := runtime.Close(context.Background())
	if runErr != nil {
		runErr = fmt.Errorf("TUI 错误: %w", runErr)
	}
	return errors.Join(runErr, closeErr)
}
