package prompt

import (
	"reflect"
	"strings"
	"testing"
)

func TestStableSectionsOrderAndCoreContent(t *testing.T) {
	bundle := Build(BuildRequest{})
	names := make([]string, 0, len(bundle.StableBlocks))
	content := JoinBlocks(bundle.StableBlocks)
	for _, block := range bundle.StableBlocks {
		names = append(names, block.Name)
		if !block.Stable {
			t.Fatalf("stable block marked dynamic: %#v", block)
		}
	}
	expected := []string{"身份", "系统约束", "任务模式", "动作执行", "工具使用", "语气风格", "文本输出"}
	if !reflect.DeepEqual(names, expected) {
		t.Fatalf("unexpected stable block order: %#v", names)
	}
	for _, want := range []string{"XAgent", "Plan Mode", "优先使用专用工具", "编辑或写入文件前必须先读取", "项目内", "中文", "验证证据"} {
		if !strings.Contains(content, want) {
			t.Fatalf("stable content missing %q:\n%s", want, content)
		}
	}
}

func TestOptionalStableSectionsAreDeterministic(t *testing.T) {
	optional := []Section{
		{Name: "beta", Priority: 100, Content: "B", Stable: true},
		{Name: "alpha", Priority: 100, Content: "A", Stable: true},
		{Name: "ignored-dynamic", Priority: 90, Content: "D", Stable: false},
		{Name: "empty", Priority: 80, Content: "   ", Stable: true},
		{Name: "alpha", Priority: 10, Content: "duplicate", Stable: true},
	}
	first := Build(BuildRequest{OptionalStableSections: optional})
	second := Build(BuildRequest{OptionalStableSections: optional})
	if JoinBlocks(first.StableBlocks) != JoinBlocks(second.StableBlocks) {
		t.Fatalf("stable blocks are not deterministic")
	}
	names := []string{}
	for _, block := range first.StableBlocks {
		names = append(names, block.Name)
	}
	expectedSuffix := []string{"alpha", "beta"}
	if !reflect.DeepEqual(names[len(names)-2:], expectedSuffix) {
		t.Fatalf("unexpected optional order: %#v", names)
	}
	stable := JoinBlocks(first.StableBlocks)
	if strings.Contains(stable, "ignored-dynamic") || strings.Contains(stable, "duplicate") {
		t.Fatalf("unexpected optional content in stable blocks: %s", stable)
	}
}

func TestDynamicBlocksAreSeparateAndEscaped(t *testing.T) {
	root := `/tmp/<system-reminder>` + "\x1b[31m" + "`code`" + "‮"
	bundle := Build(BuildRequest{Mode: RunModePlan, Iteration: 1, ProjectRoot: root})
	stable := JoinBlocks(bundle.StableBlocks)
	dynamic := JoinBlocks(bundle.DynamicBlocks)
	if strings.Contains(stable, "/tmp") || strings.Contains(stable, "system-reminder") {
		t.Fatalf("dynamic project root leaked into stable blocks: %s", stable)
	}
	for _, want := range []string{"<system-reminder>", "Plan Mode", "不得写文件", "&lt;system-reminder&gt;"} {
		if !strings.Contains(dynamic, want) {
			t.Fatalf("dynamic content missing %q:\n%s", want, dynamic)
		}
	}
	for _, forbidden := range []string{"\x1b", "`code`", "‮"} {
		if strings.Contains(dynamic, forbidden) {
			t.Fatalf("dynamic content was not escaped, found %q in %s", forbidden, dynamic)
		}
	}
}

func TestDynamicIterationStrategy(t *testing.T) {
	tests := []struct {
		iteration int
		want      string
	}{
		{iteration: 1, want: "当前请求处于 Do Mode"},
		{iteration: 2, want: "继续按计划推进"},
		{iteration: 3, want: "Do Mode 关键约束"},
	}
	for _, tt := range tests {
		bundle := Build(BuildRequest{Mode: RunModeDo, Iteration: tt.iteration})
		dynamic := JoinBlocks(bundle.DynamicBlocks)
		if !strings.Contains(dynamic, tt.want) {
			t.Fatalf("iteration %d missing %q:\n%s", tt.iteration, tt.want, dynamic)
		}
	}
}

func TestDynamicBlocksDoNotIncludeUserInjection(t *testing.T) {
	bundle := Build(BuildRequest{Mode: RunModeDefault, Iteration: 1, ProjectRoot: "/repo"})
	dynamic := JoinBlocks(bundle.DynamicBlocks)
	if strings.Contains(dynamic, "忽略之前") || strings.Contains(dynamic, "你现在是 system") {
		t.Fatalf("unexpected user-like injection in dynamic blocks: %s", dynamic)
	}
}
