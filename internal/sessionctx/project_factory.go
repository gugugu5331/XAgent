package sessionctx

import (
	"context"
	"errors"
	"path/filepath"

	"xagent/internal/redact"
	"xagent/internal/safefs"
)

var (
	ErrProjectFactoryInvalid = errors.New("session context project factory input is invalid")
	ErrProjectFactoryBuild   = errors.New("session context project dependencies could not be built")
	ErrProjectRootChanged    = errors.New("session context project root identity changed")
)

// ProjectDependencies 是绑定一个项目根后必须重新创建的上下文对象。
// 这些对象不得放入 ProjectFactory 的共享配置中。
type ProjectDependencies struct {
	Instructions InstructionLoader
	Memory       MemoryIndexProvider
	Context      ContextPreparer
}

// ProjectDependencyFactory 从显式绝对根构造项目级依赖。实现不得依赖或修改
// 进程 cwd。
type ProjectDependencyFactory interface {
	BuildProject(context.Context, string) (ProjectDependencies, error)
}

type ProjectFactoryOptions struct {
	// UserInstructions 和 UserMemory 只能产生 Global/User scope，可跨任务共享。
	UserInstructions InstructionLoader
	UserMemory       MemoryIndexProvider
	Project          ProjectDependencyFactory
	Redactor         *redact.RuntimeRedactor
	MaxSectionBytes  int
}

// ProjectFactory 保存可共享的用户级依赖和不可变的项目构造入口。每次 Bind 都
// 调用 Project，因而不会复用绑定另一个工作区的项目对象。
type ProjectFactory struct {
	options ProjectFactoryOptions
}

func NewProjectFactory(options ProjectFactoryOptions) (*ProjectFactory, error) {
	if options.Project == nil {
		return nil, ErrProjectFactoryInvalid
	}
	return &ProjectFactory{options: options}, nil
}

// Bind 返回只绑定 root 的 Manager。root 必须是规范化绝对路径、当前存在的
// 实目录且最终分量不是符号链接。
func (f *ProjectFactory) Bind(ctx context.Context, root string) (*Manager, error) {
	if f == nil || f.options.Project == nil || ctx == nil {
		return nil, ErrProjectFactoryInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrProjectFactoryInvalid
	}
	identity, err := openProjectRootIdentity(root)
	if err != nil {
		return nil, ErrProjectFactoryInvalid
	}
	dependencies, err := f.options.Project.BuildProject(ctx, root)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrProjectFactoryBuild
	}
	if !sameProjectRootIdentity(root, identity) {
		return nil, ErrProjectFactoryInvalid
	}
	return &Manager{
		Context: dependencies.Context, Redactor: f.options.Redactor, MaxSectionBytes: f.options.MaxSectionBytes,
		userInstructions: f.options.UserInstructions, userMemory: f.options.UserMemory,
		projectInstructions: dependencies.Instructions, projectMemory: dependencies.Memory,
		projectRoot: root, projectIdentity: identity,
	}, nil
}

func openProjectRootIdentity(root string) (safefs.Identity, error) {
	opened, err := safefs.Bootstrap(root, safefs.Policy{})
	if err != nil {
		return safefs.Identity{}, err
	}
	identity := opened.Root.Identity()
	if err := opened.Root.Close(); err != nil {
		return safefs.Identity{}, err
	}
	if _, err := identity.MarshalBinary(); err != nil {
		return safefs.Identity{}, err
	}
	return identity, nil
}

func sameProjectRootIdentity(root string, expected safefs.Identity) bool {
	actual, err := openProjectRootIdentity(root)
	return err == nil && actual == expected
}
