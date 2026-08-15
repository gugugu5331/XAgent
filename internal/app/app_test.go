package app

import (
	"reflect"
	"strings"
	"testing"

	"xagent/internal/artifact"
	"xagent/internal/conversation"
	"xagent/internal/hook"
	"xagent/internal/orchestrator"
)

var (
	_ ConversationAccess = (conversation.Store)(nil)
	_ Orchestration      = (*orchestrator.Orchestrator)(nil)
	_ ArtifactUserReader = (artifact.Store)(nil)
	_ HookLifecycle      = (hook.Runtime)(nil)
)

func TestTUIHasNoDomainServiceDependency(t *testing.T) {
	servicesType := reflect.TypeOf(AppServices{})
	wantFields := []string{"Conversations", "Orchestrator", "Artifacts", "Hooks"}
	if servicesType.NumField() != len(wantFields) {
		t.Fatalf("AppServices fields = %d, want %d", servicesType.NumField(), len(wantFields))
	}
	for index, wantName := range wantFields {
		field := servicesType.Field(index)
		if field.Name != wantName {
			t.Fatalf("AppServices field %d = %q, want %q", index, field.Name, wantName)
		}
		if field.Type.Kind() != reflect.Interface {
			t.Fatalf("AppServices.%s type = %v, want narrow interface", field.Name, field.Type)
		}
	}

	forbidden := []string{"config", "provider", "mcp", "registry", "executor", "manager", "store"}
	for index := 0; index < servicesType.NumField(); index++ {
		field := servicesType.Field(index)
		name := strings.ToLower(field.Name)
		for _, fragment := range forbidden {
			if strings.Contains(name, fragment) {
				t.Fatalf("AppServices exposes broad dependency field %q", field.Name)
			}
		}
	}

	assertInterfaceMethods(t, reflect.TypeOf((*ConversationAccess)(nil)).Elem(), []string{"Create", "List", "Load", "Save"})
	assertInterfaceMethods(t, reflect.TypeOf((*Orchestration)(nil)).Elem(), []string{"BuildExecutionProfile", "CompactContext", "PermissionStatus", "ResolveToolConfirmation", "SendRequest", "SendSkill", "WaitIdle"})
	assertInterfaceMethods(t, reflect.TypeOf((*ArtifactUserReader)(nil)).Elem(), []string{"OpenForUser"})
	assertInterfaceMethods(t, reflect.TypeOf((*HookLifecycle)(nil)).Elem(), []string{"SessionEnd", "SessionStart", "SystemStart"})

	for _, interfaceType := range []reflect.Type{
		reflect.TypeOf((*ConversationAccess)(nil)).Elem(),
		reflect.TypeOf((*Orchestration)(nil)).Elem(),
		reflect.TypeOf((*ArtifactUserReader)(nil)).Elem(),
		reflect.TypeOf((*HookLifecycle)(nil)).Elem(),
	} {
		assertNarrowMethodSignatures(t, interfaceType)
	}
}

func assertInterfaceMethods(t *testing.T, interfaceType reflect.Type, want []string) {
	t.Helper()
	if interfaceType.Kind() != reflect.Interface {
		t.Fatalf("%v is not an interface", interfaceType)
	}
	if interfaceType.NumMethod() != len(want) {
		t.Fatalf("%v methods = %d, want %d", interfaceType, interfaceType.NumMethod(), len(want))
	}
	for index, wantName := range want {
		if method := interfaceType.Method(index); method.Name != wantName {
			t.Fatalf("%v method %d = %q, want %q", interfaceType, index, method.Name, wantName)
		}
	}
}

func assertNarrowMethodSignatures(t *testing.T, interfaceType reflect.Type) {
	t.Helper()
	for methodIndex := 0; methodIndex < interfaceType.NumMethod(); methodIndex++ {
		method := interfaceType.Method(methodIndex)
		for inputIndex := 0; inputIndex < method.Type.NumIn(); inputIndex++ {
			assertNarrowBoundaryType(t, interfaceType, method.Name, method.Type.In(inputIndex))
		}
		for outputIndex := 0; outputIndex < method.Type.NumOut(); outputIndex++ {
			assertNarrowBoundaryType(t, interfaceType, method.Name, method.Type.Out(outputIndex))
		}
	}
}

func assertNarrowBoundaryType(t *testing.T, owner reflect.Type, method string, valueType reflect.Type) {
	t.Helper()
	for {
		switch valueType.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
			valueType = valueType.Elem()
			continue
		}
		break
	}
	packagePath := valueType.PkgPath()
	typeName := valueType.Name()
	if strings.HasSuffix(packagePath, "/config") || strings.HasSuffix(packagePath, "/provider") || strings.HasSuffix(packagePath, "/mcpclient") {
		t.Fatalf("%v.%s leaks domain service type %v", owner, method, valueType)
	}
	if strings.HasSuffix(packagePath, "/artifact") && typeName == "Store" {
		t.Fatalf("%v.%s leaks complete artifact Store", owner, method)
	}
	if strings.HasSuffix(packagePath, "/tool") && (typeName == "Registry" || typeName == "Executor" || typeName == "Tool") {
		t.Fatalf("%v.%s leaks tool capability %v", owner, method, valueType)
	}
	if strings.HasSuffix(typeName, "Manager") || typeName == "Deps" || typeName == "AppServices" {
		t.Fatalf("%v.%s leaks container or manager type %v", owner, method, valueType)
	}
	if valueType.Kind() == reflect.Interface && typeName == "" {
		t.Fatalf("%v.%s leaks anonymous interface capability %v", owner, method, valueType)
	}
}
