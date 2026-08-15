package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestSkillHistoryPolicyConstructorFixesAllCaps(t *testing.T) {
	constructorType := reflect.TypeOf(NewSkillHistoryPolicy)
	if constructorType.NumIn() != 3 || constructorType.NumOut() != 2 {
		t.Fatalf("constructor type = %v, want exactly three values and policy,error", constructorType)
	}
	for index := 0; index < constructorType.NumIn(); index++ {
		if constructorType.In(index).Kind() != reflect.Int64 {
			t.Fatalf("constructor input %d = %v, want int64", index, constructorType.In(index))
		}
	}
	if constructorType.Out(0) != reflect.TypeOf(SkillHistoryPolicy{}) || !constructorType.Out(1).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		t.Fatalf("constructor outputs = (%v,%v)", constructorType.Out(0), constructorType.Out(1))
	}

	cases := []struct {
		name                                string
		maxSession, window, margin          int64
		wantSession, wantWindow, wantMargin int64
	}{
		{name: "minimum", maxSession: 1, window: 2, margin: 1, wantSession: 1, wantWindow: 2, wantMargin: 1},
		{name: "maximum", maxSession: 1 << 30, window: 10_000_000, margin: 1_000_000, wantSession: 1 << 30, wantWindow: 10_000_000, wantMargin: 1_000_000},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			policy, err := NewSkillHistoryPolicy(testCase.maxSession, testCase.window, testCase.margin)
			if err != nil {
				t.Fatal(err)
			}
			if !policy.valid() || policy.maxTurns != 1000 || policy.maxSessionBytes != testCase.wantSession ||
				policy.modelWindowTokens != testCase.wantWindow || policy.autoMarginTokens != testCase.wantMargin {
				t.Fatalf("constructed policy = %#v", policy)
			}
		})
	}
}

func TestSkillHistoryPolicyRejectsZeroOutOfRangeAndInvalidRelations(t *testing.T) {
	if (SkillHistoryPolicy{}).valid() {
		t.Fatal("zero SkillHistoryPolicy is valid")
	}
	if err := (SkillHistoryPolicy{}).requireValid(); err == nil {
		t.Fatal("zero SkillHistoryPolicy passed secondary validation")
	}

	tests := []struct {
		name                       string
		maxSession, window, margin int64
	}{
		{name: "session zero", maxSession: 0, window: 2, margin: 1},
		{name: "session negative", maxSession: -1, window: 2, margin: 1},
		{name: "session cap plus one", maxSession: 1<<30 + 1, window: 2, margin: 1},
		{name: "session overflow", maxSession: math.MaxInt64, window: 2, margin: 1},
		{name: "window zero", maxSession: 1, window: 0, margin: 1},
		{name: "window below minimum", maxSession: 1, window: 1, margin: 1},
		{name: "window cap plus one", maxSession: 1, window: 10_000_001, margin: 1},
		{name: "window negative", maxSession: 1, window: -1, margin: 1},
		{name: "margin zero", maxSession: 1, window: 2, margin: 0},
		{name: "margin negative", maxSession: 1, window: 2, margin: -1},
		{name: "margin cap plus one", maxSession: 1, window: 1_000_002, margin: 1_000_001},
		{name: "margin equals window", maxSession: 1, window: 2, margin: 2},
		{name: "margin exceeds window", maxSession: 1, window: 2, margin: 3},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			policy, err := NewSkillHistoryPolicy(testCase.maxSession, testCase.window, testCase.margin)
			if err == nil || policy != (SkillHistoryPolicy{}) {
				t.Fatalf("invalid inputs returned (%#v, %v)", policy, err)
			}
		})
	}
}

func TestSkillHistoryPolicyCannotBeForgedOrRaised(t *testing.T) {
	typeOfPolicy := reflect.TypeOf(SkillHistoryPolicy{})
	if typeOfPolicy.Kind() != reflect.Struct || typeOfPolicy.NumField() == 0 {
		t.Fatalf("SkillHistoryPolicy type = %v", typeOfPolicy)
	}
	for index := 0; index < typeOfPolicy.NumField(); index++ {
		field := typeOfPolicy.Field(index)
		if field.IsExported() {
			t.Fatalf("SkillHistoryPolicy field %s is exported", field.Name)
		}
		switch field.Type.Kind() {
		case reflect.Int, reflect.Int64, reflect.Uint64:
		default:
			t.Fatalf("SkillHistoryPolicy field %s carries mutable capability type %v", field.Name, field.Type)
		}
	}

	constructed, err := NewSkillHistoryPolicy(1024, 4096, 512)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*SkillHistoryPolicy)
	}{
		{name: "turn cap", mutate: func(policy *SkillHistoryPolicy) { policy.maxTurns = 1001 }},
		{name: "session cap", mutate: func(policy *SkillHistoryPolicy) { policy.maxSessionBytes = 1<<30 + 1 }},
		{name: "window cap", mutate: func(policy *SkillHistoryPolicy) { policy.modelWindowTokens = 10_000_001 }},
		{name: "margin cap", mutate: func(policy *SkillHistoryPolicy) { policy.autoMarginTokens = 1_000_001 }},
		{name: "invalid relation", mutate: func(policy *SkillHistoryPolicy) { policy.autoMarginTokens = policy.modelWindowTokens }},
		{name: "marker", mutate: func(policy *SkillHistoryPolicy) { policy.marker = 0 }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			forged := constructed
			mutation.mutate(&forged)
			if forged.valid() || forged.requireValid() == nil {
				t.Fatalf("forged policy passed validation: %#v", forged)
			}
		})
	}

	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(current), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	constructors := 0
	policyTypes := 0
	compositesOutsideConstructor := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), err)
		}
		for _, declaration := range parsed.Decls {
			if general, ok := declaration.(*ast.GenDecl); ok {
				for _, spec := range general.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !strings.Contains(typeSpec.Name.Name, "SkillHistoryPolicy") {
						continue
					}
					policyTypes++
					if typeSpec.Name.Name != "SkillHistoryPolicy" {
						t.Errorf("parallel policy/options type %s is forbidden", typeSpec.Name.Name)
					}
					if _, ok := typeSpec.Type.(*ast.StructType); !ok {
						t.Errorf("SkillHistoryPolicy is replaceable through %T", typeSpec.Type)
					}
				}
			}
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			returnsPolicy := false
			if function.Type.Results != nil {
				for _, field := range function.Type.Results.List {
					if identifier, ok := field.Type.(*ast.Ident); ok && identifier.Name == "SkillHistoryPolicy" {
						returnsPolicy = true
					}
				}
			}
			if returnsPolicy {
				constructors++
				if function.Name.Name != "NewSkillHistoryPolicy" || function.Recv != nil || function.Type.Params.NumFields() != 3 {
					t.Errorf("unexpected SkillHistoryPolicy constructor %s", function.Name.Name)
				}
			}
			if function.Recv != nil {
				for _, receiver := range function.Recv.List {
					if pointer, ok := receiver.Type.(*ast.StarExpr); ok {
						if identifier, ok := pointer.X.(*ast.Ident); ok && identifier.Name == "SkillHistoryPolicy" {
							t.Errorf("SkillHistoryPolicy has mutable pointer method %s", function.Name.Name)
						}
					}
				}
			}
			if function.Body == nil || function.Name.Name == "NewSkillHistoryPolicy" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if identifier, ok := literal.Type.(*ast.Ident); ok && identifier.Name == "SkillHistoryPolicy" {
					compositesOutsideConstructor++
				}
				return true
			})
		}
	}
	if policyTypes != 1 || constructors != 1 || compositesOutsideConstructor != 0 {
		t.Fatalf("policy types=%d constructors=%d composites outside constructor=%d, want 1,1,0", policyTypes, constructors, compositesOutsideConstructor)
	}
}
