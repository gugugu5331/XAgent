package agentrole

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type ManagerOptions struct {
	Sources  []FileSource
	Plugins  ProviderRegistry
	Tools    []ToolMetadata
	Models   ModelCatalog
	Limits   Limits
	Redactor *redact.RuntimeRedactor
}

type Manager interface {
	Snapshot() Snapshot
	Resolve(name string) (ResolvedRole, bool)
	Refresh(context.Context) (RefreshResult, error)
}

type roleManager struct {
	refreshMu sync.Mutex
	current   atomic.Pointer[Snapshot]
	options   ManagerOptions
}

type diagnosticRecord struct {
	rank       int
	sourceID   string
	providerID string
	origin     string
	diagnostic diagnostics.SafeDiagnostic
}

func NewManager(ctx context.Context, options ManagerOptions) (Manager, error) {
	if ctx == nil {
		return nil, errors.New("role manager context is nil")
	}
	normalized, err := normalizeManagerOptions(options)
	if err != nil {
		return nil, err
	}
	manager := &roleManager{options: normalized}
	snapshot, err := manager.build(ctx)
	if err != nil {
		return nil, err
	}
	snapshot.Generation = 1
	manager.current.Store(&snapshot)
	return manager, nil
}

func (m *roleManager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	snapshot := m.current.Load()
	if snapshot == nil {
		return Snapshot{}
	}
	return snapshot.Clone()
}

func (m *roleManager) Resolve(name string) (ResolvedRole, bool) {
	if m == nil {
		return ResolvedRole{}, false
	}
	canonical := strings.ToLower(strings.TrimSpace(name))
	if !roleNamePattern.MatchString(canonical) {
		return ResolvedRole{}, false
	}
	snapshot := m.current.Load()
	if snapshot == nil {
		return ResolvedRole{}, false
	}
	definition, ok := snapshot.Definitions[canonical]
	if !ok {
		return ResolvedRole{}, false
	}
	return ResolvedRole{Generation: snapshot.Generation, Definition: definition.Clone()}, true
}

func (m *roleManager) Refresh(ctx context.Context) (RefreshResult, error) {
	if m == nil || ctx == nil {
		return RefreshResult{}, errors.New("role manager refresh is invalid")
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	current := m.current.Load()
	if current == nil {
		return RefreshResult{}, errors.New("role manager snapshot is unavailable")
	}
	next, err := m.build(ctx)
	if err != nil {
		base := refreshFromSnapshot(*current)
		var safeErr *diagnostics.SafeError
		if errors.As(err, &safeErr) {
			base.Error = cloneSafeError(safeErr)
			return base, nil
		}
		return base, err
	}
	if err := ctx.Err(); err != nil {
		return refreshFromSnapshot(*current), err
	}
	if next.Fingerprint == current.Fingerprint {
		result := refreshFromSnapshot(*current)
		result.Published = true
		return result, nil
	}
	if current.Generation == math.MaxUint64 {
		result := refreshFromSnapshot(*current)
		result.Error = safeManagerError(m.options.Redactor, "role_generation_exhausted", "role generation is exhausted")
		return result, nil
	}
	next.Generation = current.Generation + 1
	m.current.Store(&next)
	result := refreshFromSnapshot(next)
	result.Published = true
	result.Changed = true
	return result, nil
}

func refreshFromSnapshot(snapshot Snapshot) RefreshResult {
	cloned := snapshot.Clone()
	return RefreshResult{
		Generation:         snapshot.Generation,
		Snapshot:           cloned,
		Diagnostics:        append([]diagnostics.SafeDiagnostic(nil), snapshot.Diagnostics...),
		DiagnosticsDropped: snapshot.DiagnosticsDropped,
	}
}

func normalizeManagerOptions(options ManagerOptions) (ManagerOptions, error) {
	if options.Redactor == nil || options.Limits.Validate() != nil {
		return ManagerOptions{}, errors.New("invalid role manager options")
	}
	tools := append([]ToolMetadata(nil), options.Tools...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	toolNames := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool.Name == "" || strings.TrimSpace(tool.Name) != tool.Name || !utf8.ValidString(tool.Name) ||
			int64(len(tool.Name)) > options.Limits.MaxToolNameBytes || hasUnsafeControls(tool.Name, false) {
			return ManagerOptions{}, errors.New("invalid role manager tool metadata")
		}
		if _, duplicate := toolNames[tool.Name]; duplicate {
			return ManagerOptions{}, errors.New("duplicate role manager tool metadata")
		}
		toolNames[tool.Name] = struct{}{}
	}
	models := options.Models.Models()
	if len(models) != 4 {
		return ManagerOptions{}, errors.New("invalid role manager model catalog")
	}
	for index, model := range models {
		if index > 0 && models[index-1].Alias >= model.Alias {
			return ManagerOptions{}, errors.New("invalid role manager model metadata")
		}
		if model.Provider == "" || int64(len(model.Provider)) > options.Limits.MaxProviderIDBytes ||
			(model.Available && (model.Concrete == "" || int64(len(model.Concrete)) > options.Limits.MaxModelBytes)) {
			return ManagerOptions{}, errors.New("invalid role manager model metadata")
		}
	}

	sources := append([]FileSource(nil), options.Sources...)
	seenSources := make(map[string]struct{}, len(sources))
	for index := range sources {
		if sources[index].Source != SourceBuiltin && sources[index].Source != SourceUser && sources[index].Source != SourceProject {
			return ManagerOptions{}, errors.New("invalid role manager file source")
		}
		normalizedID, err := normalizeIdentifier(sources[index].ID, options.Limits.MaxSourceIDBytes)
		if err != nil {
			return ManagerOptions{}, errors.New("invalid role manager file source identity")
		}
		sources[index].ID = normalizedID
		identity := string(sources[index].Source) + "\x00" + normalizedID
		if _, duplicate := seenSources[identity]; duplicate {
			return ManagerOptions{}, errors.New("duplicate role manager file source")
		}
		seenSources[identity] = struct{}{}
	}
	sort.Slice(sources, func(i, j int) bool {
		leftRank, rightRank := sourceRank(sources[i].Source), sourceRank(sources[j].Source)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if sources[i].ID != sources[j].ID {
			return sources[i].ID < sources[j].ID
		}
		return sources[i].Root < sources[j].Root
	})
	if options.Plugins != nil {
		sealed, ok := options.Plugins.(interface{ isSealed() bool })
		if !ok || !sealed.isSealed() {
			return ManagerOptions{}, errors.New("role provider registry is not sealed")
		}
	}
	options.Sources = sources
	options.Tools = tools
	return options, nil
}

func (m *roleManager) build(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	definitions := make(map[string]Definition)
	diagnosticRecords := make([]diagnosticRecord, 0)
	candidateIdentities := make([]string, 0)
	toolNames := make(map[string]struct{}, len(m.options.Tools))
	for _, tool := range m.options.Tools {
		toolNames[tool.Name] = struct{}{}
	}
	candidatesUsed := 0
	var bytesUsed int64
	seenValidBySource := make(map[Source]map[string]struct{})

	consumeBatch := func(candidates []Candidate, provenance Provenance, providerBatch bool) error {
		if len(candidates) > m.options.Limits.MaxCandidates-candidatesUsed {
			return safeManagerError(m.options.Redactor, string(ErrProviderLimit), "role candidate budget was exceeded")
		}
		prepared := make([]Candidate, 0, len(candidates))
		for _, candidate := range candidates {
			if providerBatch && len(candidate.Diagnostics) > m.options.Limits.MaxDiagnostics-len(diagnosticRecords) {
				return safeManagerError(m.options.Redactor, string(ErrProviderLimit), "role provider diagnostic budget was exceeded")
			}
			if providerBatch && candidateExceedsProviderLimits(candidate, m.options.Limits) {
				return safeManagerError(m.options.Redactor, string(ErrProviderLimit), "role provider candidate budget was exceeded")
			}
			prepared = append(prepared, prepareCandidate(candidate, toolNames, m.options.Limits, m.options.Redactor))
		}
		sort.SliceStable(prepared, func(i, j int) bool {
			if prepared[i].Origin.Text() != prepared[j].Origin.Text() {
				return prepared[i].Origin.Text() < prepared[j].Origin.Text()
			}
			return prepared[i].Name < prepared[j].Name
		})
		for _, candidate := range prepared {
			if err := ctx.Err(); err != nil {
				return err
			}
			candidatesUsed++
			size, ok := retainedCandidateBytes(candidate)
			if !ok || size > m.options.Limits.MaxTotalBytes-bytesUsed {
				return safeManagerError(m.options.Redactor, string(ErrProviderLimit), "role candidate byte budget was exceeded")
			}
			bytesUsed += size
			bound := provenance
			bound.Origin = candidate.Origin
			for _, diagnostic := range candidate.Diagnostics {
				diagnosticRecords = append(diagnosticRecords, diagnosticRecord{
					rank: sourceRank(bound.Source), sourceID: bound.SourceID, providerID: bound.ProviderID,
					origin: bound.Origin.Text(), diagnostic: diagnostic,
				})
			}
			candidateIdentities = append(candidateIdentities, candidateFingerprint(candidate, bound))
			if !candidate.Valid {
				continue
			}
			seenValid := seenValidBySource[provenance.Source]
			if seenValid == nil {
				seenValid = make(map[string]struct{})
				seenValidBySource[provenance.Source] = seenValid
			}
			if _, duplicate := seenValid[candidate.Name]; duplicate {
				return safeManagerError(m.options.Redactor, "role_source_conflict", "role source contains duplicate valid names")
			}
			seenValid[candidate.Name] = struct{}{}
			definition := Definition{
				Metadata:     candidate.Metadata.clone(),
				Instructions: candidate.Instructions,
				Provenance:   bound,
			}
			definition.Fingerprint = definitionFingerprint(definition)
			definitions[definition.Name] = definition
		}
		return nil
	}

	if m.options.Plugins != nil {
		for _, registered := range m.options.Plugins.Snapshot() {
			if err := ctx.Err(); err != nil {
				return Snapshot{}, err
			}
			remaining := m.options.Limits
			remaining.MaxCandidates -= candidatesUsed
			remaining.MaxDiagnostics -= len(diagnosticRecords)
			remaining.MaxTotalBytes -= bytesUsed
			candidates, err := loadProviderRoles(ctx, registered.Provider, ProviderLoadOptions{Limits: remaining})
			if err != nil {
				if contextError := providerContextError(ctx, err); contextError != nil {
					return Snapshot{}, contextError
				}
				return Snapshot{}, providerSafeError(m.options.Redactor, err)
			}
			provenance := Provenance{Source: SourcePlugin, SourceID: "plugin", ProviderID: registered.ProviderID}
			if err := consumeBatch(candidates, provenance, true); err != nil {
				return Snapshot{}, err
			}
		}
	}
	for _, source := range m.options.Sources {
		candidates, err := DiscoverFiles(ctx, source, m.options.Limits, m.options.Redactor)
		if err != nil {
			if contextError := providerContextError(ctx, err); contextError != nil {
				return Snapshot{}, contextError
			}
			var safeErr *diagnostics.SafeError
			if errors.As(err, &safeErr) {
				return Snapshot{}, cloneSafeError(safeErr)
			}
			return Snapshot{}, safeManagerError(m.options.Redactor, "role_source_failed", "role file source failed")
		}
		if err := consumeBatch(candidates, Provenance{Source: source.Source, SourceID: source.ID}, false); err != nil {
			return Snapshot{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	diagnosticsList, dropped := finalizeDiagnostics(diagnosticRecords, m.options.Limits.MaxDiagnostics, m.options.Redactor)
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	catalog := make([]CatalogItem, 0, len(names))
	for _, name := range names {
		definition := definitions[name]
		catalog = append(catalog, CatalogItem{
			Name: name, Description: definition.Description, Source: definition.Source,
			SourceID: definition.SourceID, ProviderID: definition.ProviderID,
		})
	}
	snapshot := Snapshot{
		Catalog:            catalog,
		Definitions:        definitions,
		Diagnostics:        diagnosticsList,
		DiagnosticsDropped: dropped,
		Tools:              append([]ToolMetadata(nil), m.options.Tools...),
		Models:             m.options.Models.Models(),
	}
	snapshot.Fingerprint = snapshotFingerprint(snapshot, candidateIdentities)
	return snapshot, nil
}

func prepareCandidate(candidate Candidate, tools map[string]struct{}, limits Limits, redactor *redact.RuntimeRedactor) Candidate {
	result := candidate.clone()
	result.Description = redactor.Redact(candidate.Description.Text())
	result.Instructions = redactor.Redact(candidate.Instructions.Text())
	result.Origin = redactor.Redact(candidate.Origin.Text())
	result.Diagnostics = sanitizeDiagnostics(candidate.Diagnostics, limits, redactor)
	structureError := validateCandidateStructure(result, tools, limits)
	wasValid := result.Valid
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Severity == diagnostics.SeverityError {
			result.Valid = false
		}
	}
	if structureError != "" {
		result.Valid = false
		if wasValid || len(result.Diagnostics) == 0 {
			result.Diagnostics = append(result.Diagnostics, invalidRoleDiagnostic(redactor, "role_candidate_invalid", structureError))
		}
	}
	if !result.Valid && len(result.Diagnostics) == 0 {
		result.Diagnostics = append(result.Diagnostics, invalidRoleDiagnostic(redactor, "role_candidate_invalid", "role candidate is invalid"))
	}
	sort.Slice(result.Diagnostics, func(i, j int) bool { return diagnosticLess(result.Diagnostics[i], result.Diagnostics[j]) })
	return result
}

func validateCandidateStructure(candidate Candidate, tools map[string]struct{}, limits Limits) string {
	canonicalName := strings.ToLower(strings.TrimSpace(candidate.Name))
	if candidate.Name != canonicalName || !roleNamePattern.MatchString(candidate.Name) || int64(len(candidate.Name)) > limits.MaxNameBytes {
		return "role candidate name is not canonical"
	}
	if candidate.Description.Text() == "" || !utf8.ValidString(candidate.Description.Text()) ||
		int64(len(candidate.Description.Text())) > limits.MaxDescriptionBytes || hasUnsafeControls(candidate.Description.Text(), false) {
		return "role candidate description is invalid"
	}
	if candidate.Instructions.Text() == "" || !utf8.ValidString(candidate.Instructions.Text()) ||
		int64(len(candidate.Instructions.Text())) > limits.MaxInstructionBytes || hasUnsafeControls(candidate.Instructions.Text(), true) {
		return "role candidate instructions are invalid"
	}
	if candidate.Origin.Text() == "" || !utf8.ValidString(candidate.Origin.Text()) ||
		int64(len(candidate.Origin.Text())) > limits.MaxOriginBytes || hasUnsafeControls(candidate.Origin.Text(), false) {
		return "role candidate origin is invalid"
	}
	for _, list := range [][]string{candidate.ToolAllow, candidate.ToolDeny} {
		if list != nil {
			pointer := &list
			normalized, err := normalizeToolNames(pointer, limits)
			if err != nil || !reflect.DeepEqual(normalized, list) {
				return "role candidate tool names are not canonical"
			}
		}
		for _, name := range list {
			if _, known := tools[name]; !known {
				return "role candidate references an unknown tool"
			}
		}
	}
	if len(candidate.ToolAllow) > limits.MaxToolNames || len(candidate.ToolDeny) > limits.MaxToolNames-len(candidate.ToolAllow) {
		return "role candidate has too many tool names"
	}
	if toolListBytes(candidate.ToolAllow)+toolListBytes(candidate.ToolDeny) > limits.MaxToolListBytes {
		return "role candidate tool list exceeds its size limit"
	}
	switch candidate.Model {
	case ModelInherit, ModelHaiku, ModelSonnet, ModelOpus:
	default:
		return "role candidate model alias is invalid"
	}
	switch candidate.PermissionMode {
	case PermissionInherit, PermissionStrict, PermissionDefault, PermissionPermissive:
	default:
		return "role candidate permission mode is invalid"
	}
	if candidate.MaxIterations != nil && *candidate.MaxIterations < 0 {
		return "role candidate iteration limit is invalid"
	}
	return ""
}

func candidateExceedsProviderLimits(candidate Candidate, limits Limits) bool {
	if len(candidate.Name) > int(limits.MaxNameBytes) ||
		len(candidate.Description.Text()) > int(limits.MaxDescriptionBytes) ||
		len(candidate.Instructions.Text()) > int(limits.MaxInstructionBytes) ||
		len(candidate.Origin.Text()) > int(limits.MaxOriginBytes) {
		return true
	}
	if len(candidate.ToolAllow) > limits.MaxToolNames || len(candidate.ToolDeny) > limits.MaxToolNames-len(candidate.ToolAllow) {
		return true
	}
	var toolBytes int64
	for _, name := range append(cloneStringsPreserveNil(candidate.ToolAllow), candidate.ToolDeny...) {
		if int64(len(name)) > limits.MaxToolNameBytes || int64(len(name)) > math.MaxInt64-toolBytes {
			return true
		}
		toolBytes += int64(len(name))
	}
	if toolBytes > limits.MaxToolListBytes {
		return true
	}
	for _, diagnostic := range candidate.Diagnostics {
		if len(diagnostic.Code) > int(limits.MaxOriginBytes) || len(diagnostic.Source) > int(limits.MaxOriginBytes) ||
			len(diagnostic.Hint) > int(limits.MaxOriginBytes) || len(diagnostic.Message.Text()) > int(limits.MaxDescriptionBytes) {
			return true
		}
	}
	return false
}

func sanitizeDiagnostics(input []diagnostics.SafeDiagnostic, limits Limits, redactor *redact.RuntimeRedactor) []diagnostics.SafeDiagnostic {
	result := make([]diagnostics.SafeDiagnostic, 0, len(input))
	for _, diagnostic := range input {
		code := sanitizeDiagnosticField(diagnostic.Code, "role_candidate_invalid", limits.MaxOriginBytes, redactor)
		source := sanitizeDiagnosticField(diagnostic.Source, "agentrole", limits.MaxOriginBytes, redactor)
		hint := sanitizeDiagnosticField(diagnostic.Hint, "fix_role_definition", limits.MaxOriginBytes, redactor)
		severity := diagnostic.Severity
		if severity != diagnostics.SeverityInfo && severity != diagnostics.SeverityWarning && severity != diagnostics.SeverityError {
			severity = diagnostics.SeverityError
		}
		message := redactor.Redact(diagnostic.Message.Text())
		if message.Text() == "" || !utf8.ValidString(message.Text()) || int64(len(message.Text())) > limits.MaxDescriptionBytes || hasUnsafeControls(message.Text(), true) {
			message = redactor.Redact("role candidate diagnostic is invalid")
			severity = diagnostics.SeverityError
		}
		result = append(result, diagnostics.SafeDiagnostic{Code: code, Source: source, Hint: hint, Severity: severity, Message: message})
	}
	return result
}

func sanitizeDiagnosticField(value, fallback string, maxBytes int64, redactor *redact.RuntimeRedactor) string {
	value = strings.TrimSpace(redactor.Text(value))
	if value == "" || !utf8.ValidString(value) || int64(len(value)) > maxBytes || hasUnsafeControls(value, false) {
		return fallback
	}
	return value
}

func invalidRoleDiagnostic(redactor *redact.RuntimeRedactor, code, message string) diagnostics.SafeDiagnostic {
	return diagnostics.SafeDiagnostic{
		Code: code, Source: "agentrole", Hint: "fix_role_definition", Severity: diagnostics.SeverityError,
		Message: redactor.Redact(message),
	}
}

func retainedCandidateBytes(candidate Candidate) (int64, bool) {
	values := []string{candidate.Name, candidate.Description.Text(), candidate.Instructions.Text(), candidate.Origin.Text()}
	values = append(values, candidate.ToolAllow...)
	values = append(values, candidate.ToolDeny...)
	for _, diagnostic := range candidate.Diagnostics {
		values = append(values, diagnostic.Code, diagnostic.Source, diagnostic.Hint, string(diagnostic.Severity), diagnostic.Message.Text())
	}
	var total int64
	for _, value := range values {
		if int64(len(value)) > math.MaxInt64-total {
			return 0, false
		}
		total += int64(len(value))
	}
	return total, true
}

func finalizeDiagnostics(records []diagnosticRecord, limit int, redactor *redact.RuntimeRedactor) ([]diagnostics.SafeDiagnostic, uint64) {
	sort.Slice(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if left.rank != right.rank {
			return left.rank < right.rank
		}
		if left.sourceID != right.sourceID {
			return left.sourceID < right.sourceID
		}
		if left.providerID != right.providerID {
			return left.providerID < right.providerID
		}
		if left.origin != right.origin {
			return left.origin < right.origin
		}
		return diagnosticLess(left.diagnostic, right.diagnostic)
	})
	if len(records) <= limit {
		result := make([]diagnostics.SafeDiagnostic, len(records))
		for index := range records {
			result[index] = records[index].diagnostic
		}
		return result, 0
	}
	retained := limit - 1
	if retained < 0 {
		retained = 0
	}
	result := make([]diagnostics.SafeDiagnostic, 0, limit)
	for index := 0; index < retained; index++ {
		result = append(result, records[index].diagnostic)
	}
	result = append(result, diagnostics.SafeDiagnostic{
		Code: "role_diagnostics_truncated", Source: "agentrole", Hint: "inspect_role_sources",
		Severity: diagnostics.SeverityWarning, Message: redactor.Redact("additional role diagnostics were dropped"),
	})
	return result, uint64(len(records) - retained)
}

func diagnosticLess(left, right diagnostics.SafeDiagnostic) bool {
	leftValues := []string{left.Code, left.Source, left.Hint, string(left.Severity), left.Message.Text()}
	rightValues := []string{right.Code, right.Source, right.Hint, string(right.Severity), right.Message.Text()}
	for index := range leftValues {
		if leftValues[index] != rightValues[index] {
			return leftValues[index] < rightValues[index]
		}
	}
	return false
}

func sourceRank(source Source) int {
	switch source {
	case SourcePlugin:
		return 0
	case SourceBuiltin:
		return 1
	case SourceUser:
		return 2
	case SourceProject:
		return 3
	default:
		return 4
	}
}

func providerContextError(ctx context.Context, err error) error {
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func loadProviderRoles(ctx context.Context, provider SourceProvider, options ProviderLoadOptions) (candidates []Candidate, err error) {
	defer func() {
		if recover() != nil {
			candidates = nil
			err = errors.New("role provider panicked")
		}
	}()
	return provider.LoadRoles(ctx, options)
}

func providerSafeError(redactor *redact.RuntimeRedactor, err error) *diagnostics.SafeError {
	code := string(ErrProviderFailed)
	var safeErr *diagnostics.SafeError
	if errors.As(err, &safeErr) && (safeErr.Code == string(ErrProviderLimit) || safeErr.Code == string(ErrProviderFailed)) {
		code = safeErr.Code
	}
	message := "role provider failed"
	if code == string(ErrProviderLimit) {
		message = "role provider exceeded a resource limit"
	}
	return safeManagerError(redactor, code, message)
}

func safeManagerError(redactor *redact.RuntimeRedactor, code, message string) *diagnostics.SafeError {
	return &diagnostics.SafeError{Code: code, Source: "agentrole", Message: redactor.Redact(message), Recoverable: false}
}

func cloneSafeError(err *diagnostics.SafeError) *diagnostics.SafeError {
	if err == nil {
		return nil
	}
	cloned := *err
	return &cloned
}
