package orchestrator

import (
	"context"
	"fmt"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/hook"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

// PrepareTUISubagentSubmit captures the current committed conversation and
// effective prompt at the App's serialized command boundary. The returned
// context is valid only for the synchronous Service.Submit -> Prepare call;
// no mutable parent state or global snapshot table is retained.
func (o *Orchestrator) PrepareTUISubagentSubmit(
	ctx context.Context,
	conv *conversation.Conversation,
	mode RunMode,
	input subagent.SubmitInput,
) (context.Context, subagent.SubmitInput, error) {
	if o == nil || ctx == nil || conv == nil {
		return nil, subagent.SubmitInput{}, o.tuiParentSnapshotError()
	}
	profile, err := o.buildExecutionProfile(mode, nil, 0)
	if err != nil {
		return nil, subagent.SubmitInput{}, o.tuiParentSnapshotError()
	}
	registry, err := o.registryForProfile(mode, profile)
	if err != nil || registry == nil || !registry.IsSealed() {
		return nil, subagent.SubmitInput{}, o.tuiParentSnapshotError()
	}
	prefix, err := o.captureTUIParentPrompt(ctx, conv, mode, profile, registry)
	if err != nil {
		return nil, subagent.SubmitInput{}, o.tuiParentSnapshotError()
	}
	runtime, err := o.CaptureParentRuntimeSnapshot(ParentRuntimeCaptureInput{
		Conversation:  conv,
		Prompt:        prefix,
		Registry:      registry,
		Mode:          mode,
		Profile:       profile,
		ParentContext: ctx,
	})
	if err != nil {
		return nil, subagent.SubmitInput{}, o.tuiParentSnapshotError()
	}

	generation := o.requestGeneration.Add(1)
	if generation == 0 {
		return nil, subagent.SubmitInput{}, o.tuiParentSnapshotError()
	}
	parent := subagent.ParentRef{
		ConversationID:    conv.ID,
		ExecutionID:       fmt.Sprintf("tui-request-%d", generation),
		RequestGeneration: generation,
	}
	submitCtx, err := WithParentRuntimeSnapshot(ctx, parent, runtime)
	if err != nil {
		return nil, subagent.SubmitInput{}, o.tuiParentSnapshotError()
	}
	input.Origin = subagent.OriginTUI
	input.Parent = parent
	input.Invocation = subagent.InvocationRef{}
	return submitCtx, input, nil
}

func (o *Orchestrator) captureTUIParentPrompt(
	ctx context.Context,
	conv *conversation.Conversation,
	mode RunMode,
	profile skill.ExecutionProfile,
	registry *tool.Registry,
) (provider.PromptPrefixSnapshot, error) {
	prepareOptions := contextmgr.PrepareOptions{
		Mode:             contextmgr.ModeAuto,
		PersistArtifacts: profile.Persist,
		Observer: hookCompactionObserver{
			runtime: o.hookRuntime(), binding: hook.CompactBinding{SessionID: conv.ID}, redact: o.redactText,
		},
	}
	optionalSections := []prompt.Section{}
	changed := false
	if o.sessionContext != nil {
		var prepared sessionctx.PreparedContext
		var err error
		if options, ok := o.sessionContext.(optionsSessionPreparer); ok {
			prepared, err = options.PrepareWithOptions(ctx, conv, prepareOptions)
		} else {
			prepared, err = o.sessionContext.Prepare(ctx, conv, sessionctx.PrepareAuto)
		}
		if err != nil {
			return provider.PromptPrefixSnapshot{}, err
		}
		optionalSections = append(optionalSections, prepared.StableSections...)
		changed = prepared.MessagesChanged
		if o.diagnostics != nil {
			o.diagnostics.Add(prepared.Diagnostics...)
		}
	} else if o.contextManager != nil {
		result, err := o.contextManager.PrepareWithOptions(ctx, conv, prepareOptions)
		if err != nil {
			return provider.PromptPrefixSnapshot{}, err
		}
		changed = result.Changed
	}
	if changed && profile.Persist && o.store != nil {
		if _, err := o.store.Save(ctx, conv); err != nil {
			return provider.PromptPrefixSnapshot{}, err
		}
	}

	bundle := prompt.Build(prompt.BuildRequest{
		Mode:                   promptRunMode(mode),
		Iteration:              1,
		ProjectRoot:            o.projectRoot(),
		SkillCatalog:           skill.CatalogPromptWithRedactor(profile.Catalog, o.redactText),
		ActiveSkills:           skill.ActivePromptWithRedactor(profile.Activity, o.redactText),
		OptionalStableSections: optionalSections,
	})
	request := provider.ChatRequest{
		Model:         profile.Model,
		StableSystem:  o.providerStableBlocks(bundle.StableBlocks),
		DynamicSystem: o.providerDynamicBlocks(bundle.DynamicBlocks),
		Messages:      o.providerMessages(conv.Messages),
		Tools:         toolDefinitionsFromRegistry(registry),
		Thinking:      o.thinking,
		Cache:         provider.CachePolicy{EnablePromptCache: true, CacheTools: true},
	}
	if bundle.UsesOrderedBlocks() {
		request.System = o.providerOrderedBlocks(bundle.OrderedBlocks)
		request.StableSystem = nil
		request.DynamicSystem = nil
		request.Cache.SystemBreakpointName = bundle.SystemBreakpointName
		request.Cache.CacheTools = false
	}
	if err := requestContextError(ctx); err != nil {
		return provider.PromptPrefixSnapshot{}, err
	}
	return provider.CapturePromptPrefix(request)
}

func (o *Orchestrator) tuiParentSnapshotError() error {
	message := "TUI parent runtime snapshot is unavailable"
	if o != nil && o.runtimeRedactor != nil {
		return subagent.SafeError(subagent.ErrParentSnapshotUnavailable, o.runtimeRedactor.Redact(message), true)
	}
	return subagent.SafeError(subagent.ErrParentSnapshotUnavailable, redact.NewRuntimeRedactor().Redact(message), true)
}
