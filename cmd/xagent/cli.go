package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"xagent/internal/config"
)

const (
	exitSuccess = 0
	exitFailure = 1
	exitUsage   = 2
)

const publicUsage = `Usage:
  xagent [--config <path>]
  xagent --help
  xagent --version

Options:
  --config <path>  使用指定配置文件
  -h, --help       显示帮助
  --version        显示版本
`

type cliAction uint8

const (
	cliRun cliAction = iota
	cliHelp
	cliVersion
)

type cliInvocation struct {
	action         cliAction
	configPath     string
	explicitConfig bool
}

type cliRuntime func([]string) error

// buildAssemblyFromCLI is the production CLI seam consumed by the later
// atomic entry-point cutover. It is intentionally not called by runArgs until
// T4.29a; keeping that switch separate prevents an unpublished Runtime from
// becoming the interactive production entry prematurely.
func buildAssemblyFromCLI(args []string) (*Runtime, error) {
	options, err := assemblyOptionsFromSystem(args)
	if err != nil {
		return nil, err
	}
	return defaultAssembly().Build(context.Background(), options)
}

// assemblyOptionsFromSystem is the sole normal-CLI adapter from operating
// system directories and the normalized --config argument to Assembly's
// explicit input contract. It does not expose factories or any transport
// customization: production always uses the system certificate pool.
func assemblyOptionsFromSystem(args []string) (AssemblyOptions, error) {
	configPath, err := parseConfigPath(args)
	if err != nil {
		return AssemblyOptions{}, err
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("项目目录错误: %w", err)
	}
	projectRoot, err = absoluteAssemblyCLIPath(projectRoot)
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("项目目录错误: %w", err)
	}
	projectConfig, err := absoluteAssemblyCLIPath(configPath)
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("配置路径错误: %w", err)
	}
	userConfigBase, err := os.UserConfigDir()
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("用户配置目录错误: %w", err)
	}
	userConfigBase, err = absoluteAssemblyCLIPath(userConfigBase)
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("用户配置目录错误: %w", err)
	}
	userCacheBase, err := os.UserCacheDir()
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("用户缓存目录错误: %w", err)
	}
	userCacheBase, err = absoluteAssemblyCLIPath(userCacheBase)
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("用户缓存目录错误: %w", err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("用户主目录错误: %w", err)
	}
	userHome, err = absoluteAssemblyCLIPath(userHome)
	if err != nil {
		return AssemblyOptions{}, fmt.Errorf("用户主目录错误: %w", err)
	}

	userConfigRoot := filepath.Join(userConfigBase, "xagent")
	userConfigPath := filepath.Join(userConfigRoot, config.DefaultConfigFile)
	if userConfigPath == projectConfig {
		userConfigPath = ""
	} else if _, err := os.Stat(userConfigPath); errors.Is(err, os.ErrNotExist) {
		userConfigPath = ""
	} else if err != nil {
		return AssemblyOptions{}, fmt.Errorf("用户配置错误: %w", err)
	}

	return AssemblyOptions{
		Paths: RuntimePaths{
			ProjectRoot:    projectRoot,
			UserConfigRoot: userConfigRoot,
			UserDataRoot:   filepath.Join(userHome, ".xagent"),
			UserCacheRoot:  filepath.Join(userCacheBase, "xagent"),
		},
		Config: ConfigInputs{
			UserPath:    userConfigPath,
			ProjectPath: projectConfig,
		},
		LookupEnv:    os.LookupEnv,
		TrustedRoots: nil,
		Stdin:        os.Stdin,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
	}, nil
}

func absoluteAssemblyCLIPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func runCLI(args []string, stdout io.Writer, stderr io.Writer, runtime cliRuntime) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	invocation, err := parseCLIArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "参数错误: 参数无效")
		_, _ = fmt.Fprintln(stderr)
		_, _ = io.WriteString(stderr, publicUsage)
		return exitUsage
	}
	switch invocation.action {
	case cliHelp:
		_, _ = io.WriteString(stdout, publicUsage)
		return exitSuccess
	case cliVersion:
		_, _ = io.WriteString(stdout, versionOutput())
		return exitSuccess
	case cliRun:
		if runtime == nil {
			_, _ = fmt.Fprintln(stderr, "运行错误: 启动入口不可用")
			return exitFailure
		}
		runtimeArgs := invocation.runtimeArgs()
		if err := runtime(runtimeArgs); err != nil {
			_, _ = fmt.Fprintf(stderr, "运行错误: %v\n", err)
			return exitFailure
		}
		return exitSuccess
	default:
		_, _ = fmt.Fprintln(stderr, "运行错误: 未知启动状态")
		return exitFailure
	}
}

func parseCLIArgs(args []string) (cliInvocation, error) {
	if len(args) == 1 {
		switch args[0] {
		case "-h", "--help":
			return cliInvocation{action: cliHelp}, nil
		case "--version":
			return cliInvocation{action: cliVersion}, nil
		}
	}

	invocation := cliInvocation{action: cliRun, configPath: config.DefaultConfigFile}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--":
			if index != len(args)-1 {
				return cliInvocation{}, fmt.Errorf("positional arguments are not supported")
			}
		case argument == "-h" || argument == "--help" || argument == "--version":
			return cliInvocation{}, fmt.Errorf("help and version must be used alone")
		case argument == "--config" || argument == "-config":
			if invocation.explicitConfig || index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return cliInvocation{}, fmt.Errorf("invalid config argument")
			}
			invocation.explicitConfig = true
			invocation.configPath = args[index+1]
			index++
		case strings.HasPrefix(argument, "--config=") || strings.HasPrefix(argument, "-config="):
			separator := strings.IndexByte(argument, '=')
			if invocation.explicitConfig || separator < 0 || strings.TrimSpace(argument[separator+1:]) == "" {
				return cliInvocation{}, fmt.Errorf("invalid config argument")
			}
			invocation.explicitConfig = true
			invocation.configPath = argument[separator+1:]
		default:
			return cliInvocation{}, fmt.Errorf("unsupported argument")
		}
	}
	return invocation, nil
}

func (invocation cliInvocation) runtimeArgs() []string {
	if !invocation.explicitConfig {
		return nil
	}
	return []string{"--config", invocation.configPath}
}

func parseConfigPath(args []string) (string, error) {
	invocation, err := parseCLIArgs(args)
	if err != nil {
		return "", err
	}
	if invocation.action != cliRun {
		return "", fmt.Errorf("non-runtime CLI action")
	}
	return invocation.configPath, nil
}
