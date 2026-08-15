package command

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type Registry struct {
	definitions []Definition
	lookup      map[string]int
	bindings    map[bindingKey]Binding
	help        []HelpMetadata
}

// HelpCommandMetadata is the capability-free public projection of one command.
// It intentionally excludes Handler and hidden compatibility definitions.
type HelpCommandMetadata struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
	Type        Type
	ArgHint     string
	Badge       string
}

// HelpCatalog is the immutable single-source input for user help renderers.
type HelpCatalog struct {
	Commands []HelpCommandMetadata
	Bindings []Binding
	Entries  []HelpMetadata
}

type bindingKey struct {
	context ShortcutContext
	key     string
}

type helpKey struct {
	kind HelpEntryKind
	name string
}

func New(definitions ...Definition) (*Registry, error) {
	items := make([]Definition, len(definitions))
	for index, definition := range definitions {
		item, err := normalizeDefinition(definition)
		if err != nil {
			return nil, fmt.Errorf("命令定义 %d 无效: %w", index+1, err)
		}
		items[index] = item
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	lookup := make(map[string]int)
	bindings := make(map[bindingKey]Binding)
	help := make([]HelpMetadata, 0)
	helpOwners := make(map[helpKey]string)
	type owner struct {
		name  string
		index int
	}
	owners := make(map[string]owner)
	for index, item := range items {
		keys := append([]string{item.Name}, item.Aliases...)
		for _, key := range keys {
			if previous, exists := owners[key]; exists {
				return nil, fmt.Errorf("命令名称或别名冲突 %q: /%s（定义 %d）与 /%s（定义 %d）", key, previous.name, previous.index+1, item.Name, index+1)
			}
			owners[key] = owner{name: item.Name, index: index}
			lookup[key] = index
		}
		for _, shortcut := range item.Shortcuts {
			key := bindingKey{context: shortcut.Context, key: shortcut.Key}
			if previous, exists := bindings[key]; exists {
				return nil, fmt.Errorf("快捷键冲突 %q/%q: /%s 与 /%s", shortcut.Context, shortcut.Key, previous.CanonicalName, item.Name)
			}
			bindings[key] = Binding{Context: shortcut.Context, Key: shortcut.Key, Intent: shortcut.Intent, CanonicalName: item.Name, Description: shortcut.Description}
		}
		for _, entry := range item.HelpEntries {
			key := helpKey{kind: entry.Kind, name: entry.Name}
			if previous, exists := helpOwners[key]; exists {
				return nil, fmt.Errorf("帮助元数据冲突 %q/%q: /%s 与 /%s", entry.Kind, entry.Name, previous, item.Name)
			}
			helpOwners[key] = item.Name
			help = append(help, HelpMetadata{Kind: entry.Kind, Name: entry.Name, Description: entry.Description, CanonicalName: item.Name})
		}
	}
	sort.Slice(help, func(i, j int) bool {
		if help[i].Kind != help[j].Kind {
			return help[i].Kind < help[j].Kind
		}
		if help[i].Name != help[j].Name {
			return help[i].Name < help[j].Name
		}
		return help[i].CanonicalName < help[j].CanonicalName
	})
	return &Registry{definitions: items, lookup: lookup, bindings: bindings, help: help}, nil
}

func MustNew(definitions ...Definition) *Registry {
	registry, err := New(definitions...)
	if err != nil {
		panic(err)
	}
	return registry
}

func (r *Registry) Visible() []Definition {
	if r == nil {
		return nil
	}
	visible := make([]Definition, 0, len(r.definitions))
	for _, definition := range r.definitions {
		if !definition.Hidden {
			visible = append(visible, cloneDefinition(definition))
		}
	}
	return visible
}

// Definitions returns an immutable snapshot of all registered command
// definitions, including hidden compatibility commands.
func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	definitions := make([]Definition, len(r.definitions))
	for index, definition := range r.definitions {
		definitions[index] = cloneDefinition(definition)
	}
	return definitions
}

// IntentForCommand resolves a canonical command name or alias without
// executing a Handler. App consumes this pure value at its navigation boundary.
func (r *Registry) IntentForCommand(name string) (IntentKind, bool) {
	if r == nil {
		return "", false
	}
	index, ok := r.lookup[normalizeName(name)]
	if !ok || r.definitions[index].Intent == "" {
		return "", false
	}
	return r.definitions[index].Intent, true
}

// IntentForShortcut resolves only the supplied screen context and normalized
// key, preventing a binding from leaking into another screen.
func (r *Registry) IntentForShortcut(context ShortcutContext, key string) (IntentKind, bool) {
	if r == nil {
		return "", false
	}
	binding, ok := r.bindings[bindingKey{context: context, key: normalizeShortcutKey(key)}]
	if !ok {
		return "", false
	}
	return binding.Intent, true
}

// Bindings returns a deterministic immutable snapshot for TUI/help consumers.
func (r *Registry) Bindings() []Binding {
	if r == nil {
		return nil
	}
	bindings := make([]Binding, 0, len(r.bindings))
	for _, binding := range r.bindings {
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Context != bindings[j].Context {
			return bindings[i].Context < bindings[j].Context
		}
		if bindings[i].Key != bindings[j].Key {
			return bindings[i].Key < bindings[j].Key
		}
		return bindings[i].CanonicalName < bindings[j].CanonicalName
	})
	return bindings
}

// HelpMetadata returns the single normalized source used by the later help
// renderer for permission modes and public status/diagnostics discovery.
func (r *Registry) HelpMetadata() []HelpMetadata {
	if r == nil {
		return nil
	}
	return append([]HelpMetadata(nil), r.help...)
}

// HelpCatalog returns all and only public command, shortcut, permission-mode,
// status, and diagnostics metadata. Every slice is a defensive copy.
func (r *Registry) HelpCatalog() HelpCatalog {
	if r == nil {
		return HelpCatalog{}
	}
	visible := r.Visible()
	commands := make([]HelpCommandMetadata, 0, len(visible))
	for _, definition := range visible {
		commands = append(commands, HelpCommandMetadata{
			Name: definition.Name, Aliases: append([]string(nil), definition.Aliases...),
			Description: definition.Description, Usage: definition.Usage, Type: definition.Type,
			ArgHint: definition.ArgHint, Badge: definition.Badge,
		})
	}
	return HelpCatalog{Commands: commands, Bindings: r.Bindings(), Entries: r.HelpMetadata()}
}

func normalizeDefinition(definition Definition) (Definition, error) {
	definition.Name = normalizeName(definition.Name)
	if definition.Name == "" {
		return Definition{}, fmt.Errorf("名称不能为空")
	}
	if containsWhitespace(definition.Name) {
		return Definition{}, fmt.Errorf("名称不能包含空白: %q", definition.Name)
	}
	if !validType(definition.Type) {
		return Definition{}, fmt.Errorf("命令类型无效: %q", definition.Type)
	}
	if definition.Handler == nil {
		return Definition{}, fmt.Errorf("/%s 缺少处理函数", definition.Name)
	}
	if definition.Intent != "" && !validIntent(definition.Intent) {
		return Definition{}, fmt.Errorf("/%s 的 intent 无效: %q", definition.Name, definition.Intent)
	}
	if definition.Hidden && (len(definition.Shortcuts) > 0 || len(definition.HelpEntries) > 0) {
		return Definition{}, fmt.Errorf("/%s 的隐藏命令不能发布快捷键或帮助元数据", definition.Name)
	}
	aliases := make([]string, len(definition.Aliases))
	seen := map[string]struct{}{definition.Name: {}}
	for index, alias := range definition.Aliases {
		alias = normalizeName(alias)
		if alias == "" {
			return Definition{}, fmt.Errorf("/%s 的别名不能为空", definition.Name)
		}
		if containsWhitespace(alias) {
			return Definition{}, fmt.Errorf("/%s 的别名不能包含空白: %q", definition.Name, alias)
		}
		if _, exists := seen[alias]; exists {
			return Definition{}, fmt.Errorf("/%s 内部名称或别名重复: %q", definition.Name, alias)
		}
		seen[alias] = struct{}{}
		aliases[index] = alias
	}
	definition.Aliases = aliases
	shortcuts := make([]Shortcut, len(definition.Shortcuts))
	seenShortcuts := make(map[bindingKey]struct{}, len(definition.Shortcuts))
	for index, shortcut := range definition.Shortcuts {
		if !validShortcutContext(shortcut.Context) {
			return Definition{}, fmt.Errorf("/%s 的快捷键上下文无效: %q", definition.Name, shortcut.Context)
		}
		shortcut.Key = normalizeShortcutKey(shortcut.Key)
		if shortcut.Key == "" || containsWhitespace(shortcut.Key) {
			return Definition{}, fmt.Errorf("/%s 的快捷键无效: %q", definition.Name, shortcut.Key)
		}
		if shortcut.Intent == "" {
			shortcut.Intent = definition.Intent
		}
		if !validIntent(shortcut.Intent) {
			return Definition{}, fmt.Errorf("/%s 的快捷键 intent 无效: %q", definition.Name, shortcut.Intent)
		}
		shortcut.Description = strings.TrimSpace(shortcut.Description)
		if shortcut.Description == "" {
			return Definition{}, fmt.Errorf("/%s 的快捷键说明不能为空", definition.Name)
		}
		key := bindingKey{context: shortcut.Context, key: shortcut.Key}
		if _, exists := seenShortcuts[key]; exists {
			return Definition{}, fmt.Errorf("/%s 的快捷键重复: %q/%q", definition.Name, shortcut.Context, shortcut.Key)
		}
		seenShortcuts[key] = struct{}{}
		shortcuts[index] = shortcut
	}
	definition.Shortcuts = shortcuts
	helpEntries := make([]HelpEntry, len(definition.HelpEntries))
	seenHelp := make(map[helpKey]struct{}, len(definition.HelpEntries))
	for index, entry := range definition.HelpEntries {
		entry.Name = strings.ToLower(strings.TrimSpace(entry.Name))
		entry.Description = strings.TrimSpace(entry.Description)
		if !validHelpEntry(entry) {
			return Definition{}, fmt.Errorf("/%s 的帮助元数据无效: %q/%q", definition.Name, entry.Kind, entry.Name)
		}
		key := helpKey{kind: entry.Kind, name: entry.Name}
		if _, exists := seenHelp[key]; exists {
			return Definition{}, fmt.Errorf("/%s 的帮助元数据重复: %q/%q", definition.Name, entry.Kind, entry.Name)
		}
		seenHelp[key] = struct{}{}
		helpEntries[index] = entry
	}
	definition.HelpEntries = helpEntries
	definition.Description = strings.TrimSpace(definition.Description)
	definition.Usage = strings.TrimSpace(definition.Usage)
	definition.ArgHint = strings.TrimSpace(definition.ArgHint)
	definition.Badge = strings.TrimSpace(definition.Badge)
	return definition, nil
}

func normalizeName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "/")
	return strings.ToLower(value)
}

func containsWhitespace(value string) bool {
	return strings.IndexFunc(value, unicode.IsSpace) >= 0
}

func validType(value Type) bool {
	return value == TypeLocal || value == TypeUI || value == TypePrompt
}

func validIntent(value IntentKind) bool {
	return value == IntentNewConversation || value == IntentShowSessions || value == IntentOpenConversation || value == IntentQuit || value == IntentCancel
}

func validShortcutContext(value ShortcutContext) bool {
	return value == ShortcutChatIdle || value == ShortcutChatStreaming || value == ShortcutChatConfirmation || value == ShortcutSessions
}

func validHelpEntry(entry HelpEntry) bool {
	if entry.Description == "" {
		return false
	}
	switch entry.Kind {
	case HelpEntryPermissionMode:
		return entry.Name == "strict" || entry.Name == "default" || entry.Name == "permissive"
	case HelpEntryStatus, HelpEntryDiagnostics:
		return entry.Name == "status"
	default:
		return false
	}
}

func normalizeShortcutKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func cloneDefinition(definition Definition) Definition {
	definition.Aliases = append([]string(nil), definition.Aliases...)
	definition.Shortcuts = append([]Shortcut(nil), definition.Shortcuts...)
	definition.HelpEntries = append([]HelpEntry(nil), definition.HelpEntries...)
	return definition
}
