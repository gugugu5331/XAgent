package hook

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func testToolEvent(t *testing.T) *frozenEvent {
	f := testFactory(t, Limits{})
	c := f.executionBase(EventToolBefore, ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault})
	c.Tool = &ToolContext{CallID: "c", Name: "Bash\n", Arguments: map[string]any{"n": json.Number("1.0"), "flag": true, "path": "src/main.go"}}
	return f.freeze(c)
}

func TestNormalizedNumberEquality(t *testing.T) {
	for _, item := range []struct {
		left  string
		right string
		equal bool
	}{
		{left: "1", right: "1.0", equal: true},
		{left: "+1", right: "1.", equal: true},
		{left: ".5", right: "0.50", equal: true},
		{left: "-0", right: "+0.000e99", equal: true},
		{left: "123e-2", right: "1.23", equal: true},
		{left: "1e2", right: "100", equal: true},
		{left: "1000e-3", right: "1", equal: true},
		{left: "0.001e3", right: "1", equal: true},
		{left: "-90071992547409931234567890.125", right: "-90071992547409931234567890125e-3", equal: true},
		{left: "1", right: "-1", equal: false},
		{left: "1.01", right: "1.001", equal: false},
		{left: "1e100", right: "10e100", equal: false},
		{left: ".", right: "0", equal: false},
		{left: "1e", right: "1", equal: false},
	} {
		if got := equalNormalizedNumber(item.left, item.right); got != item.equal {
			t.Fatalf("equalNormalizedNumber(%q,%q) = %v", item.left, item.right, got)
		}
	}
	hugeNines := strings.Repeat("9", 4096)
	hugeNinesMinusOne := hugeNines[:len(hugeNines)-1] + "8"
	if !equalNormalizedNumber("1e"+hugeNines, "10e"+hugeNinesMinusOne) {
		t.Fatal("huge positive exponent carry did not compare exactly")
	}
	hugePower := "1" + strings.Repeat("0", 4096)
	if !equalNormalizedNumber("1e-"+hugeNines, "10e-"+hugePower) {
		t.Fatal("huge negative exponent borrow did not compare exactly")
	}
	if equalNormalizedNumber("1e"+hugeNines, "1e"+hugeNinesMinusOne) {
		t.Fatal("distinct huge exponents compared equal")
	}
}

func TestConditionHugeMantissaWithoutCanonicalCopy(t *testing.T) {
	const zeroCount = 8 << 20
	compressedOne := "1" + strings.Repeat("0", zeroCount) + "e-" + strconv.Itoa(zeroCount)
	c := EventContext{Tool: &ToolContext{Arguments: map[string]any{"n": json.Number(compressedOne)}}}
	event := &frozenEvent{value: c}
	condition, err := compileCondition(EventToolBefore, &ConditionGroup{All: []Predicate{{Field: "tool.arguments.n", Match: MatchExact, Value: json.Number("1")}}})
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := condition.matches(event); err != nil || !matched {
		t.Fatalf("compressed huge mantissa = %v,%v", matched, err)
	}

	nonMatching := strings.Repeat("1", zeroCount)
	event.value.Tool.Arguments["n"] = json.Number(nonMatching)
	if matched, err := condition.matches(event); err != nil || matched {
		t.Fatalf("significant huge mantissa = %v,%v", matched, err)
	}
}

func TestYAMLDecimalPredicateCompatibility(t *testing.T) {
	for _, item := range []struct {
		yaml   string
		actual string
	}{
		{yaml: "+1", actual: "1"},
		{yaml: ".5", actual: "0.5"},
		{yaml: "1.", actual: "1"},
	} {
		t.Run(item.yaml, func(t *testing.T) {
			var document yaml.Node
			if err := yaml.Unmarshal([]byte("value: "+item.yaml+"\n"), &document); err != nil {
				t.Fatal(err)
			}
			valueNode := document.Content[0].Content[1]
			value, err := predicateScalar(valueNode)
			if err != nil {
				t.Fatalf("tag=%s value=%q: %v", valueNode.Tag, valueNode.Value, err)
			}
			condition, err := compileCondition(EventToolBefore, &ConditionGroup{All: []Predicate{{Field: "tool.arguments.n", Match: MatchExact, Value: value}}})
			if err != nil {
				t.Fatal(err)
			}
			event := &frozenEvent{value: EventContext{Tool: &ToolContext{Arguments: map[string]any{"n": json.Number(item.actual)}}}}
			if matched, err := condition.matches(event); err != nil || !matched {
				t.Fatalf("matched = %v,%v", matched, err)
			}
		})
	}
}

func TestConditionExact(t *testing.T) {
	event := testToolEvent(t)
	cases := []struct {
		value any
		field string
		want  bool
	}{{json.Number("1"), "tool.arguments.n", true}, {"1", "tool.arguments.n", false}, {true, "tool.arguments.flag", true}, {"Bash\n", "tool.name", true}, {"bash\n", "tool.name", false}}
	for _, item := range cases {
		c, err := compileCondition(EventToolBefore, &ConditionGroup{All: []Predicate{{Field: item.field, Match: MatchExact, Value: item.value}}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.matches(event)
		if err != nil || got != item.want {
			t.Fatalf("%s %#v = %v,%v", item.field, item.value, got, err)
		}
	}
}

func TestConditionGroups(t *testing.T) {
	event := testToolEvent(t)
	all, _ := compileCondition(EventToolBefore, &ConditionGroup{All: []Predicate{{Field: "tool.arguments.flag", Match: MatchExact, Value: true}, {Field: "tool.arguments.path", Match: MatchGlob, Value: "src/*.go"}}})
	if ok, err := all.matches(event); err != nil || !ok {
		t.Fatalf("all %v %v", ok, err)
	}
	any, _ := compileCondition(EventToolBefore, &ConditionGroup{Any: []Predicate{{Field: "tool.name", Match: MatchExact, Value: "no"}, {Field: "tool.arguments.path", Match: MatchRegex, Value: `src/.*\.go`}}})
	if ok, err := any.matches(event); err != nil || !ok {
		t.Fatalf("any %v %v", ok, err)
	}
}

func TestConditionFullValueMatch(t *testing.T) {
	event := testToolEvent(t)
	for name, item := range map[string]struct {
		p    Predicate
		want bool
	}{
		"regex full":     {Predicate{Field: "tool.name", Match: MatchRegex, Value: "Bash"}, false},
		"regex newline":  {Predicate{Field: "tool.name", Match: MatchRegex, Value: "Bash\\n"}, true},
		"missing negate": {Predicate{Field: "tool.arguments.absent", Match: MatchExact, Value: "x", Negate: true}, false},
		"valid negate":   {Predicate{Field: "tool.arguments.path", Match: MatchExact, Value: "other", Negate: true}, true},
	} {
		t.Run(name, func(t *testing.T) {
			condition, err := compileCondition(EventToolBefore, &ConditionGroup{All: []Predicate{item.p}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := condition.matches(event)
			if err != nil || got != item.want {
				t.Fatalf("got %v,%v want %v", got, err, item.want)
			}
		})
	}
}

func TestEventFieldCatalog(t *testing.T) {
	if _, err := compileField(EventTurnEnd, "turn.status"); err != nil {
		t.Fatal(err)
	}
	if _, err := compileField(EventTurnStart, "turn.status"); err == nil {
		t.Fatal("future field accepted")
	}
	if _, err := compileField(EventToolBefore, "tool.arguments.deep.value"); err != nil {
		t.Fatal(err)
	}
	if _, err := compileField(EventMessageBefore, "tool.arguments.deep"); err == nil {
		t.Fatal("dynamic path outside tool accepted")
	}
}
