package repoaudit

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"xagent/internal/conversation"
)

var _ conversation.Store = (*conversation.JSONLStore)(nil)

func TestLegacyStoreMethodsHaveNoProductionReference(t *testing.T) {
	assertConversationStoreContract(t)

	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve conversation reference audit source failed")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	forbiddenNames := map[string]bool{
		"ConversationStore": true,
		"RecoveringStore":   true,
		"ConversationV2":    true,
		"MessageV2":         true,
		"ToolStateV2":       true,
		"ContextMetadataV2": true,
		"RecoveryReportV2":  true,
	}
	forbiddenStoreMethods := map[string]bool{
		"Recover": true,
		"Cleanup": true,
	}

	var findings []string
	for _, tree := range []string{"cmd", "internal"} {
		root := filepath.Join(repositoryRoot, tree)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(repositoryRoot, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			conversationAliases := map[string]bool{}
			for _, specification := range parsed.Imports {
				importPath, unquoteErr := strconv.Unquote(specification.Path.Value)
				if unquoteErr != nil {
					return unquoteErr
				}
				if importPath != "xagent/internal/conversation" {
					continue
				}
				alias := "conversation"
				if specification.Name != nil {
					alias = specification.Name.Name
				}
				if alias != "_" && alias != "." {
					conversationAliases[alias] = true
				}
			}
			var ancestors []ast.Node
			ast.Inspect(parsed, func(node ast.Node) bool {
				if node == nil {
					ancestors = ancestors[:len(ancestors)-1]
					return true
				}
				var parent ast.Node
				if len(ancestors) > 0 {
					parent = ancestors[len(ancestors)-1]
				}
				ancestors = append(ancestors, node)
				switch typed := node.(type) {
				case *ast.Ident:
					if parsed.Name.Name == "conversation" && forbiddenNames[typed.Name] {
						findings = append(findings, relative+": legacy identifier "+typed.Name)
					}
					if parsed.Name.Name == "conversation" && typed.Name == "NewFileStore" && !isSelectorName(parent, typed) {
						findings = append(findings, relative+": legacy identifier "+typed.Name)
					}
					if typed.Name == "progress" && parsed.Name.Name == "conversation" {
						findings = append(findings, relative+": message-count progress state")
					}
				case *ast.FuncDecl:
					if parsed.Name.Name == "conversation" && legacyStoreReceiver(typed.Recv) && forbiddenStoreMethods[typed.Name.Name] {
						findings = append(findings, relative+": legacy store method "+typed.Name.Name)
					}
				case *ast.SelectorExpr:
					qualifier, ok := typed.X.(*ast.Ident)
					if ok && conversationAliases[qualifier.Name] && (forbiddenNames[typed.Sel.Name] || typed.Sel.Name == "NewFileStore") {
						findings = append(findings, relative+": legacy conversation selector "+typed.Sel.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("audit Conversation production references in %s: %v", tree, err)
		}
	}
	sort.Strings(findings)
	for _, finding := range findings {
		t.Error(finding)
	}
}

func isSelectorName(parent ast.Node, identifier *ast.Ident) bool {
	selector, ok := parent.(*ast.SelectorExpr)
	return ok && selector.Sel == identifier
}

func legacyStoreReceiver(receiver *ast.FieldList) bool {
	if receiver == nil || len(receiver.List) != 1 {
		return false
	}
	typeExpression := receiver.List[0].Type
	if pointer, ok := typeExpression.(*ast.StarExpr); ok {
		typeExpression = pointer.X
	}
	identifier, ok := typeExpression.(*ast.Ident)
	if !ok {
		return false
	}
	switch identifier.Name {
	case "ConversationStore", "RecoveringStore", "FileStore", "JSONLStore":
		return true
	default:
		return false
	}
}

func assertConversationStoreContract(t *testing.T) {
	t.Helper()
	store := reflect.TypeOf((*conversation.Store)(nil)).Elem()
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	conversationPointer := reflect.TypeOf((*conversation.Conversation)(nil))
	errorType := reflect.TypeOf((*error)(nil)).Elem()

	want := map[string]struct {
		inputs  []reflect.Type
		outputs []reflect.Type
	}{
		"Create":   {[]reflect.Type{contextType}, []reflect.Type{conversationPointer, errorType}},
		"List":     {[]reflect.Type{contextType}, []reflect.Type{reflect.TypeOf(conversation.ListResult{}), errorType}},
		"Load":     {[]reflect.Type{contextType, reflect.TypeOf("")}, []reflect.Type{reflect.TypeOf(conversation.LoadResult{}), errorType}},
		"Save":     {[]reflect.Type{contextType, conversationPointer}, []reflect.Type{reflect.TypeOf(conversation.SaveResult{}), errorType}},
		"Maintain": {[]reflect.Type{contextType}, []reflect.Type{reflect.TypeOf(conversation.MaintenanceResult{}), errorType}},
	}
	if store.NumMethod() != len(want) {
		t.Fatalf("conversation.Store methods = %d, want exactly %d", store.NumMethod(), len(want))
	}
	for name, expected := range want {
		method, ok := store.MethodByName(name)
		if !ok {
			t.Errorf("conversation.Store is missing %s", name)
			continue
		}
		if method.Type.NumIn() != len(expected.inputs) || method.Type.NumOut() != len(expected.outputs) {
			t.Errorf("conversation.Store.%s has unexpected arity: %v", name, method.Type)
			continue
		}
		for index, input := range expected.inputs {
			if method.Type.In(index) != input {
				t.Errorf("conversation.Store.%s input %d = %v, want %v", name, index, method.Type.In(index), input)
			}
		}
		for index, output := range expected.outputs {
			if method.Type.Out(index) != output {
				t.Errorf("conversation.Store.%s output %d = %v, want %v", name, index, method.Type.Out(index), output)
			}
		}
	}
}
