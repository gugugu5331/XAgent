package memory

import (
	"fmt"
	"strings"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const IndexFileName = "MEMORY.md"

type IndexEntry struct {
	ID    string
	Title string
	Type  NoteType
	Scope Scope
	Path  string
	Body  string
}

type Index struct {
	Scope   Scope
	Entries []IndexEntry
}

func BuildIndex(scope Scope, notes []Note, maxLines int, maxBytes int) (Index, []diagnostics.Diagnostic) {
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxBytes <= 0 {
		maxBytes = 25 * 1024
	}
	all := make([]IndexEntry, 0, len(notes))
	for _, note := range notes {
		note = sanitizeNote(note)
		all = append(all, IndexEntry{ID: note.ID, Title: note.Title, Type: note.Type, Scope: note.Scope, Path: noteFileName(note.ID), Body: note.Body})
	}
	kept := make([]IndexEntry, 0, len(all))
	text := "# Memory Index\n\n"
	lineCount := 2
	for _, entry := range all {
		line := indexLine(entry)
		candidate := text + line + "\n"
		if lineCount+1 > maxLines || len([]byte(candidate)) > maxBytes {
			break
		}
		kept = append(kept, entry)
		text = candidate
		lineCount++
	}
	if len(all) > 0 && len(kept) == 0 {
		return Index{Scope: scope}, []diagnostics.Diagnostic{diagnostics.New("memory_index_single_note_too_large", diagnostics.SeverityWarning, "单条记忆导致索引超过限制，已拒绝写入").Safe(redact.Text)}
	}
	index := Index{Scope: scope, Entries: kept}
	if len(kept) != len(all) {
		return index, []diagnostics.Diagnostic{diagnostics.New("memory_index_truncated", diagnostics.SeverityWarning, "记忆索引超过限制，已截断低优先级条目").Safe(redact.Text)}
	}
	return index, nil
}

func MarshalIndex(index Index, maxLines int, maxBytes int) string {
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxBytes <= 0 {
		maxBytes = 25 * 1024
	}
	var builder strings.Builder
	builder.WriteString("# Memory Index\n\n")
	count := 2
	for _, entry := range index.Entries {
		line := indexLine(entry)
		candidate := builder.String() + line + "\n"
		if count+1 > maxLines || len([]byte(candidate)) > maxBytes {
			break
		}
		builder.WriteString(line)
		builder.WriteByte('\n')
		count++
	}
	return builder.String()
}

func indexLine(entry IndexEntry) string {
	return fmt.Sprintf("- [%s](%s) — %s", redact.Text(strings.TrimSpace(entry.Title)), noteFileName(entry.ID), redact.Text(summary(entry.Body)))
}

func ParseIndex(scope Scope, data []byte) (Index, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return Index{Scope: scope}, nil
	}
	if !strings.HasPrefix(text, "# Memory Index") {
		return Index{}, fmt.Errorf("记忆索引格式无效")
	}
	index := Index{Scope: scope}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- [") {
			continue
		}
		titleEnd := strings.Index(line, "](")
		pathEnd := strings.Index(line, ")")
		if titleEnd < 3 || pathEnd <= titleEnd+2 {
			continue
		}
		title := line[3:titleEnd]
		path := line[titleEnd+2 : pathEnd]
		id := strings.TrimSuffix(strings.TrimSuffix(path, ".md"), filepathSuffix(path))
		if id == "" {
			id = safeID(strings.TrimSuffix(path, ".md"))
		}
		index.Entries = append(index.Entries, IndexEntry{ID: id, Title: title, Scope: scope, Path: path})
	}
	return index, nil
}

func exceedsIndexLimit(text string, maxLines int, maxBytes int) bool {
	return len(strings.Split(strings.TrimRight(text, "\n"), "\n")) > maxLines || len([]byte(text)) > maxBytes
}

func summary(body string) string {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\n", " "))
	runes := []rune(body)
	if len(runes) > 90 {
		return string(runes[:90]) + "..."
	}
	return body
}

func filepathSuffix(path string) string {
	if strings.Contains(path, "/") {
		parts := strings.Split(path, "/")
		return strings.Join(parts[:len(parts)-1], "/") + "/"
	}
	return ""
}
