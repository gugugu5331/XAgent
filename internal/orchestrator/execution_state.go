package orchestrator

import (
	"context"
	"fmt"

	"xagent/internal/skill"
	"xagent/internal/tool"
)

type executionState struct {
	activity *skill.Activity
	profile  skill.ExecutionProfile
}

func (o *Orchestrator) newExecutionState(req RunRequest) (*executionState, error) {
	activity := req.Activity
	if activity == nil {
		activity = skill.NewActivity()
	}
	state := &executionState{activity: activity, profile: req.Profile.Clone()}
	if profileIsUnset(state.profile) {
		profile, err := o.buildExecutionProfile(req.Mode, activity, 0)
		if err != nil {
			return nil, err
		}
		state.profile = profile
	}
	return state, nil
}

func profileIsUnset(profile skill.ExecutionProfile) bool {
	return len(profile.Catalog) == 0 && len(profile.Activity.Active) == 0 && profile.Model == "" && profile.AllowedTools == nil && len(profile.ReadRoots) == 0 && profile.IndependentDepth == 0 && !profile.Persist && !profile.UpdateMemory
}

func (o *Orchestrator) buildExecutionProfile(mode RunMode, activity *skill.Activity, depth int) (skill.ExecutionProfile, error) {
	snapshot := skill.Snapshot{}
	if o.skillManager != nil {
		snapshot = o.skillManager.Snapshot()
	}
	return o.buildExecutionProfileWithSnapshot(mode, snapshot, activity, depth)
}

func (o *Orchestrator) buildExecutionProfileWithSnapshot(mode RunMode, snapshot skill.Snapshot, activity *skill.Activity, depth int) (skill.ExecutionProfile, error) {
	if activity == nil {
		activity = skill.NewActivity()
	}
	baseTools := []string{}
	if o.registry != nil {
		baseTools = o.registry.Names()
	}
	profile, err := skill.BuildProfile(skill.ProfileRequest{
		Snapshot:         snapshot,
		Activity:         activity.Snapshot(),
		DefaultModel:     o.defaultModel,
		BaseTools:        baseTools,
		ReadOnly:         mode == RunModePlan,
		IndependentDepth: depth,
	})
	if err != nil {
		return skill.ExecutionProfile{}, err
	}
	if o.registry != nil {
		if _, ok := o.registry.Get(tool.LoadSkillToolName); !ok {
			if o.skillManager != nil {
				return skill.ExecutionProfile{}, fmt.Errorf("系统工具 %q 未注册", tool.LoadSkillToolName)
			}
			delete(profile.AllowedTools, tool.LoadSkillToolName)
		}
	}
	return profile, nil
}

func (o *Orchestrator) BuildExecutionProfile(mode RunMode, activity *skill.Activity, depth int) (skill.ExecutionProfile, error) {
	return o.buildExecutionProfile(mode, activity, depth)
}

func (o *Orchestrator) registryForProfile(mode RunMode, profile skill.ExecutionProfile) (*tool.Registry, error) {
	if o.registry == nil {
		return nil, nil
	}
	always := []string{}
	if _, ok := o.registry.Get(tool.LoadSkillToolName); ok {
		always = append(always, tool.LoadSkillToolName)
	}
	return o.registry.View(tool.ViewOptions{
		AllowedNames:  profile.AllowedTools,
		AlwaysInclude: always,
		ReadOnly:      mode == RunModePlan,
	})
}

func (o *Orchestrator) contextWithReadScope(ctx context.Context, profile skill.ExecutionProfile) (context.Context, error) {
	if o.executor == nil || len(profile.ReadRoots) == 0 {
		return ctx, nil
	}
	scope, err := tool.NewPinnedReadScope(o.executor.ProjectRoot, profile.ReadRoots)
	if err != nil {
		return nil, err
	}
	return tool.WithReadScope(ctx, scope), nil
}

func (o *Orchestrator) refreshExecutionProfile(mode RunMode, state *executionState) error {
	if state == nil {
		return fmt.Errorf("execution state is nil")
	}
	profile, err := o.buildExecutionProfile(mode, state.activity, state.profile.IndependentDepth)
	if err != nil {
		return err
	}
	profile.Persist = state.profile.Persist
	profile.UpdateMemory = state.profile.UpdateMemory
	state.profile = profile
	return nil
}
