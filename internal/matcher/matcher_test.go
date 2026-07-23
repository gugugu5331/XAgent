package matcher

import "testing"

func TestMatchExactUsesWholeUnmodifiedValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		pattern string
		want    bool
	}{
		{name: "equal", value: "Bash", pattern: "Bash", want: true},
		{name: "case sensitive", value: "bash", pattern: "Bash"},
		{name: "no trim value", value: " Bash", pattern: "Bash"},
		{name: "no trim pattern", value: "Bash", pattern: "Bash "},
		{name: "empty", value: "", pattern: "", want: true},
		{name: "newline is data", value: "a\nb", pattern: "a\nb", want: true},
		{name: "whole value", value: "prefix-value-suffix", pattern: "value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := MatchExact(test.value, test.pattern); got != test.want {
				t.Fatalf("MatchExact(%q, %q) = %v, want %v", test.value, test.pattern, got, test.want)
			}
		})
	}
}

func TestMatchGlobUsesWholeUnmodifiedValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		pattern string
		want    bool
	}{
		{name: "star", value: "git status", pattern: "git *", want: true},
		{name: "whole value", value: "prefix git status", pattern: "git *"},
		{name: "question", value: "file.go", pattern: "file.?o", want: true},
		{name: "case sensitive", value: "FILE.go", pattern: "file.*"},
		{name: "no trim", value: " file.go", pattern: "file.*"},
		{name: "single star does not cross slash", value: "a/b.go", pattern: "*.go"},
		{name: "double star crosses slash", value: "a/b.go", pattern: "**/*.go", want: true},
		{name: "double star can match zero directories", value: "b.go", pattern: "**/*.go", want: true},
		{name: "character class", value: "file2.go", pattern: "**/file[0-9].go", want: true},
		{name: "class with leading bracket", value: "]/file.go", pattern: "**/[]a]/file.go", want: true},
		{name: "escaped unicode", value: "你/file.go", pattern: `**/\你/file.go`, want: true},
		{name: "newline remains matchable", value: "a\nb", pattern: "**", want: true},
		{name: "empty", value: "", pattern: "", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := MatchGlob(test.value, test.pattern)
			if err != nil {
				t.Fatalf("MatchGlob(%q, %q): %v", test.value, test.pattern, err)
			}
			if got != test.want {
				t.Fatalf("MatchGlob(%q, %q) = %v, want %v", test.value, test.pattern, got, test.want)
			}
		})
	}
}

func TestMatchGlobRejectsInvalidPattern(t *testing.T) {
	for _, pattern := range []string{"[", "**/[abc", `trailing\`} {
		if _, err := MatchGlob("anything", pattern); err == nil {
			t.Fatalf("MatchGlob accepted invalid pattern %q", pattern)
		}
	}
}

func TestMatchIsDeterministic(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		if !MatchExact("value", "value") {
			t.Fatal("exact result changed")
		}
		matched, err := MatchGlob("internal/deep/file.go", "internal/**/*.go")
		if err != nil || !matched {
			t.Fatalf("glob result changed: matched=%v err=%v", matched, err)
		}
	}
}
