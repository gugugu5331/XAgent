package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"xagent/internal/diagnostics"
)

type SnapshotOptions struct {
	Generation       uint64
	ToolNames        []string
	ReservedCommands []string
	Diagnostics      []diagnostics.Diagnostic
	Redact           func(string) string
}

func BuildSnapshot(definitions []Definition, options SnapshotOptions) (Snapshot, error) {
	byTier := map[Source]map[string]Definition{
		SourceBuiltin: {},
		SourceUser:    {},
		SourceProject: {},
	}
	for _, original := range definitions {
		if !validSource(original.Source) {
			return Snapshot{}, fmt.Errorf("definition has unknown skill source")
		}
		metadata, err := ValidateMetadata(original.Metadata)
		if err != nil {
			return Snapshot{}, fmt.Errorf("invalid definition metadata")
		}
		definition := cloneDefinition(original)
		definition.Metadata = metadata
		tier := byTier[definition.Source]
		if previous, exists := tier[definition.Name]; exists {
			firstPath := safeValue(previous.EntryPath, options.Redact)
			secondPath := safeValue(definition.EntryPath, options.Redact)
			return Snapshot{}, fmt.Errorf("duplicate skill %q in %s source: %s and %s", definition.Name, definition.Source, firstPath, secondPath)
		}
		tier[definition.Name] = definition
	}

	effective := make(map[string]Definition)
	for _, source := range []Source{SourceBuiltin, SourceUser, SourceProject} {
		names := make([]string, 0, len(byTier[source]))
		for name := range byTier[source] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			effective[name] = cloneDefinition(byTier[source][name])
		}
	}

	toolSet := make(map[string]struct{}, len(options.ToolNames))
	for _, toolName := range options.ToolNames {
		toolName = strings.TrimSpace(toolName)
		if toolName == "" {
			continue
		}
		toolSet[toolName] = struct{}{}
	}
	effectiveNames := make([]string, 0, len(effective))
	for name := range effective {
		effectiveNames = append(effectiveNames, name)
	}
	sort.Strings(effectiveNames)
	for _, name := range effectiveNames {
		definition := effective[name]
		for _, toolName := range definition.AllowedTools {
			if _, exists := toolSet[toolName]; !exists {
				return Snapshot{}, fmt.Errorf("skill %q references unknown tool %q", name, safeValue(toolName, options.Redact))
			}
		}
	}

	reserved := map[string]struct{}{LoadSkillToolName: {}}
	for _, name := range options.ReservedCommands {
		name = normalizeCommandName(name)
		if name != "" {
			reserved[name] = struct{}{}
		}
	}
	diagnosticItems := append([]diagnostics.Diagnostic(nil), options.Diagnostics...)
	catalog := make([]CatalogItem, 0, len(effectiveNames))
	for _, name := range effectiveNames {
		definition := effective[name]
		_, conflict := reserved[name]
		catalog = append(catalog, CatalogItem{
			Name:         name,
			Description:  safeValue(definition.Description, options.Redact),
			Mode:         definition.Mode,
			SlashEnabled: !conflict,
		})
		if conflict {
			diagnosticItems = append(diagnosticItems, diagnostics.New(
				"skill_command_reserved",
				diagnostics.SeverityWarning,
				fmt.Sprintf("skill %q conflicts with a reserved command and has no slash command", name),
			).WithPath(safeValue(definition.EntryPath, options.Redact)).WithSource(string(definition.Source)).Safe(options.Redact))
		}
	}
	sortDiagnostics(diagnosticItems)
	generation := options.Generation
	if generation == 0 {
		generation = 1
	}
	snapshot := Snapshot{
		Generation:  generation,
		Catalog:     catalog,
		Definitions: effective,
		Diagnostics: diagnosticItems,
	}
	snapshot.Fingerprint = snapshotFingerprint(snapshot)
	return snapshot.Clone(), nil
}

func buildManagerSnapshot(ctx context.Context, options ManagerOptions, generation uint64) (Snapshot, error) {
	definitions := make([]Definition, 0)
	diagnosticItems := make([]diagnostics.Diagnostic, 0)
	for _, source := range options.Sources {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		discovered, sourceDiagnostics, err := discoverContext(ctx, source, options.Limits, options.Redact)
		if err != nil {
			return Snapshot{}, err
		}
		definitions = append(definitions, discovered...)
		diagnosticItems = append(diagnosticItems, sourceDiagnostics...)
	}
	return BuildSnapshot(definitions, SnapshotOptions{
		Generation:       generation,
		ToolNames:        options.ToolNames,
		ReservedCommands: options.ReservedCommand,
		Diagnostics:      diagnosticItems,
		Redact:           options.Redact,
	})
}

func snapshotFingerprint(snapshot Snapshot) string {
	hash := sha256.New()
	names := make([]string, 0, len(snapshot.Definitions))
	for name := range snapshot.Definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		definition := snapshot.Definitions[name]
		for _, value := range []string{
			name,
			definition.Description,
			string(definition.Mode),
			strconv.Itoa(definition.History),
			definition.Model,
			strings.Join(definition.AllowedTools, "\x00"),
			definition.Body,
			definition.EntryPath,
			definition.PackageRoot,
			string(definition.Source),
			definition.Fingerprint,
		} {
			io.WriteString(hash, value)
			io.WriteString(hash, "\x00")
		}
	}
	for _, diagnostic := range snapshot.Diagnostics {
		for _, value := range []string{diagnostic.Code, diagnostic.Message, diagnostic.Source, diagnostic.Path, string(diagnostic.Severity)} {
			io.WriteString(hash, value)
			io.WriteString(hash, "\x00")
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func normalizeCommandName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "/")
	return name
}
