package workspace

import (
	"errors"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"xagent/internal/proctree"
	"xagent/internal/safefs"
)

const maxReadonlyRoots = 253

// Protection 同时拥有 safefs Roots、任务写 capability 和子进程保护计划工厂。
// safefs ProtectionPlan 借用 Roots，因此关闭顺序必须先停掉全部消费者，再关闭
// Protection；Close 会逆序关闭所有 Root，使 plan 和 capability 一并失效。
type Protection struct {
	mu sync.Mutex

	working     WritableRoot
	scratch     WritableRoot
	artifact    WritableRoot
	readonly    []ReadonlyRoot
	plan        *safefs.ProtectionPlan
	processPlan proctree.ProtectionPlanFactory
	owned       []*safefs.Root
	closed      bool
	closeErr    error
}

func OpenProtection(paths ProtectionPaths) (*Protection, error) {
	canonical, err := validateProtectionPaths(paths)
	if err != nil {
		return nil, err
	}

	owned := make([]*safefs.Root, 0, 3+len(canonical.Readonly))
	rollback := func() {
		for index := len(owned) - 1; index >= 0; index-- {
			_ = owned[index].Close()
		}
	}
	open := func(path string, policy safefs.Policy) (safefs.OpenResult, error) {
		result, openErr := safefs.Bootstrap(path, policy)
		if openErr != nil {
			return safefs.OpenResult{}, ErrProtectionInvalid
		}
		owned = append(owned, result.Root)
		return result, nil
	}

	// .git 与任务控制目录从普通写 capability 中排除。保护整个 .xagent 槽，
	// 同时覆盖尚不存在、因此无法安全预开中间句柄的 .xagent/worktrees。
	working, err := open(canonical.Working, safefs.Policy{ReadonlySlots: []string{".git", ".control", ".xagent"}})
	if err != nil {
		rollback()
		return nil, err
	}
	scratch, err := open(canonical.Scratch, safefs.Policy{})
	if err != nil {
		rollback()
		return nil, err
	}
	artifact, err := open(canonical.Artifact, safefs.Policy{})
	if err != nil {
		rollback()
		return nil, err
	}
	readonlyResults := make([]safefs.OpenResult, 0, len(canonical.Readonly))
	for _, path := range canonical.Readonly {
		result, openErr := open(path, safefs.Policy{})
		if openErr != nil {
			rollback()
			return nil, openErr
		}
		readonlyResults = append(readonlyResults, result)
	}

	writableRoots := []*safefs.Root{working.Root, scratch.Root, artifact.Root}
	readonlyRoots := make([]*safefs.Root, len(readonlyResults))
	for index := range readonlyResults {
		readonlyRoots[index] = readonlyResults[index].Root
	}
	plan, err := safefs.NewProtectionPlan(safefs.ProtectionPlanOptions{
		Writable: writableRoots,
		Readonly: readonlyRoots,
	})
	if err != nil {
		rollback()
		return nil, ErrProtectionInvalid
	}
	workingCapabilities, err := plan.Capabilities(working.Root)
	if err != nil {
		rollback()
		return nil, ErrProtectionInvalid
	}
	scratchCapabilities, err := plan.Capabilities(scratch.Root)
	if err != nil {
		rollback()
		return nil, ErrProtectionInvalid
	}
	artifactCapabilities, err := plan.Capabilities(artifact.Root)
	if err != nil {
		rollback()
		return nil, ErrProtectionInvalid
	}
	allReadableRoots := make([]*safefs.Root, 0, len(writableRoots)+len(readonlyRoots))
	allReadableRoots = append(allReadableRoots, writableRoots...)
	allReadableRoots = append(allReadableRoots, readonlyRoots...)
	processPlan, err := proctree.NewProtectionPlanFactory(allReadableRoots, canonical.Scratch)
	if err != nil {
		rollback()
		return nil, ErrProtectionInvalid
	}

	protection := &Protection{
		working: WritableRoot{
			Path: canonical.Working, Root: working.Root, Capabilities: workingCapabilities,
		},
		scratch: WritableRoot{
			Path: canonical.Scratch, Root: scratch.Root, Capabilities: scratchCapabilities,
		},
		artifact: WritableRoot{
			Path: canonical.Artifact, Root: artifact.Root, Capabilities: artifactCapabilities,
		},
		plan:        plan,
		processPlan: processPlan,
		owned:       owned,
		readonly:    make([]ReadonlyRoot, len(readonlyResults)),
	}
	for index, result := range readonlyResults {
		protection.readonly[index] = ReadonlyRoot{Path: canonical.Readonly[index], Root: result.Root}
	}
	if err := protection.Verify(); err != nil {
		_ = protection.Close()
		return nil, err
	}
	return protection, nil
}

func validateProtectionPaths(paths ProtectionPaths) (ProtectionPaths, error) {
	canonical := ProtectionPaths{
		Working:  paths.Working,
		Scratch:  paths.Scratch,
		Artifact: paths.Artifact,
		Readonly: append([]string(nil), paths.Readonly...),
	}
	// Worktree Runtime 必须至少把主工作区作为只读 Root 绑定；没有该边界时
	// 任务保护计划是不完整的，不能以“无额外依赖”解释为空列表。
	if len(canonical.Readonly) == 0 || len(canonical.Readonly) > maxReadonlyRoots {
		return ProtectionPaths{}, ErrProtectionInvalid
	}
	all := make([]string, 0, 3+len(canonical.Readonly))
	all = append(all, canonical.Working, canonical.Scratch, canonical.Artifact)
	all = append(all, canonical.Readonly...)
	seen := make(map[string]struct{}, len(all))
	for _, path := range all {
		if path == "" || !utf8.ValidString(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return ProtectionPaths{}, ErrProtectionInvalid
		}
		if _, duplicate := seen[path]; duplicate {
			return ProtectionPaths{}, ErrProtectionInvalid
		}
		seen[path] = struct{}{}
	}
	return canonical, nil
}

func (p *Protection) Working() WritableRoot {
	if p == nil {
		return WritableRoot{}
	}
	return p.working
}

func (p *Protection) Scratch() WritableRoot {
	if p == nil {
		return WritableRoot{}
	}
	return p.scratch
}

func (p *Protection) Artifact() WritableRoot {
	if p == nil {
		return WritableRoot{}
	}
	return p.artifact
}

func (p *Protection) Readonly() []ReadonlyRoot {
	if p == nil {
		return nil
	}
	return append([]ReadonlyRoot(nil), p.readonly...)
}

func (p *Protection) Plan() *safefs.ProtectionPlan {
	if p == nil {
		return nil
	}
	return p.plan
}

func (p *Protection) ProcessPlans() proctree.ProtectionPlanFactory {
	if p == nil {
		return nil
	}
	return p.processPlan
}

// Verify 重新打开每个绝对路径并按平台对象身份比对，拒绝路径替换、symlink
// 和已关闭句柄。授权本身仍由持有的 no-follow Root 完成，不依赖字符串前缀。
func (p *Protection) Verify() error {
	if p == nil {
		return ErrProtectionInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.plan == nil || len(p.owned) != 3+len(p.readonly) {
		return ErrProtectionInvalid
	}
	for _, writable := range []WritableRoot{p.working, p.scratch, p.artifact} {
		if err := verifyRootPath(writable.Path, writable.Root); err != nil {
			return err
		}
	}
	for _, readonly := range p.readonly {
		if err := verifyRootPath(readonly.Path, readonly.Root); err != nil {
			return err
		}
		if err := p.plan.ProbeReadonlyPath(readonly.Path); err != nil {
			return ErrProtectionInvalid
		}
	}
	return nil
}

func verifyRootPath(path string, expected *safefs.Root) error {
	if expected == nil || expected.Identity() == (safefs.Identity{}) {
		return ErrProtectionInvalid
	}
	opened, err := safefs.Bootstrap(path, safefs.Policy{})
	if err != nil {
		return ErrProtectionInvalid
	}
	identity := opened.Root.Identity()
	closeErr := opened.Root.Close()
	if closeErr != nil || identity == (safefs.Identity{}) || identity != expected.Identity() {
		return ErrProtectionInvalid
	}
	return nil
}

func (p *Protection) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	var failures []error
	for index := len(p.owned) - 1; index >= 0; index-- {
		if err := p.owned[index].Close(); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		p.closeErr = errors.New("workspace protection close failed")
	}
	return p.closeErr
}
