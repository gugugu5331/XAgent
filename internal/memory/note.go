package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"xagent/internal/redact"
	"xagent/internal/safefs"
)

type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

type NoteType string

const (
	NoteUserPreference   NoteType = "user_preference"
	NoteCorrection       NoteType = "correction"
	NoteProjectKnowledge NoteType = "project_knowledge"
	NoteReference        NoteType = "reference"
)

type Note struct {
	ID         string
	Title      string
	Type       NoteType
	Scope      Scope
	Source     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Supersedes []string
	Body       string
}

type ProjectIdentity struct {
	RootRealPath string
	ConfigHash   string
	ID           string
	rootObject   safefs.Identity
}

func NewProjectIdentity(projectRoot string, configDigest string) (ProjectIdentity, error) {
	root := strings.TrimSpace(projectRoot)
	if root == "" {
		return ProjectIdentity{}, fmt.Errorf("项目根目录不能为空")
	}
	realRoot, err := canonicalProjectExistingRoot(root)
	if err != nil {
		return ProjectIdentity{}, fmt.Errorf("解析项目根目录失败: %w", err)
	}
	opened, err := safefs.Bootstrap(realRoot, safefs.Policy{})
	if err != nil {
		return ProjectIdentity{}, fmt.Errorf("打开项目根目录失败")
	}
	rootObject := opened.Root.Identity()
	objectIdentity, identityErr := rootObject.MarshalBinary()
	closeErr := opened.Root.Close()
	if identityErr != nil || closeErr != nil {
		return ProjectIdentity{}, fmt.Errorf("读取项目根身份失败")
	}
	normalized := normalizeProjectPath(realRoot)
	configHash := hashText(strings.TrimSpace(configDigest))
	id := hashText(normalized + "\x00" + hex.EncodeToString(objectIdentity) + "\x00" + configHash)
	return ProjectIdentity{RootRealPath: realRoot, ConfigHash: configHash, ID: id, rootObject: rootObject}, nil
}

func (identity ProjectIdentity) matchesLiveRoot() bool {
	canonical, err := canonicalProjectExistingRoot(identity.RootRealPath)
	if err != nil || canonical != identity.RootRealPath {
		return false
	}
	opened, err := safefs.Bootstrap(identity.RootRealPath, safefs.Policy{})
	if err != nil {
		return false
	}
	actual := opened.Root.Identity()
	return opened.Root.Close() == nil && actual == identity.rootObject
}

func canonicalProjectExistingRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil || !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("project path is not absolute")
	}
	resolved = filepath.Clean(resolved)
	volume := filepath.VolumeName(resolved)
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	remainder := strings.TrimPrefix(resolved, current)
	if remainder == "" {
		return current, nil
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." || filepath.Base(component) != component {
			return "", fmt.Errorf("project path is invalid")
		}
		candidateInfo, err := os.Lstat(filepath.Join(current, component))
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", err
		}
		actual := ""
		for _, entry := range entries {
			entryInfo, infoErr := entry.Info()
			if infoErr == nil && os.SameFile(candidateInfo, entryInfo) {
				actual = entry.Name()
				break
			}
		}
		if actual == "" {
			return "", fmt.Errorf("project path identity is unavailable")
		}
		current = filepath.Join(current, actual)
	}
	return filepath.Clean(current), nil
}

func normalizeProjectPath(path string) string {
	path = filepath.ToSlash(filepath.Clean(path))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		path = strings.ToLower(path)
	}
	return path
}

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func NewNote(noteType NoteType, scope Scope, title string, body string, source string, now time.Time) Note {
	cleanTitle := strings.TrimSpace(redact.Text(title))
	cleanBody := strings.TrimSpace(redact.Text(body))
	if cleanTitle == "" {
		cleanTitle = "未命名记忆"
	}
	if now.IsZero() {
		now = time.Now()
	}
	id := noteID(noteType, scope, cleanTitle, cleanBody, now)
	return Note{ID: id, Title: cleanTitle, Type: noteType, Scope: scope, Source: redact.Text(strings.TrimSpace(source)), CreatedAt: now, UpdatedAt: now, Body: cleanBody}
}

func MarshalNote(note Note) string {
	note = sanitizeNote(note)
	var builder strings.Builder
	builder.WriteString("---\n")
	for _, line := range noteFrontmatterLines(note) {
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	builder.WriteString("---\n\n")
	builder.WriteString(strings.TrimSpace(note.Body))
	builder.WriteByte('\n')
	return builder.String()
}

func ParseNote(data []byte) (Note, error) {
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "---\n") {
		return Note{}, fmt.Errorf("note 缺少 frontmatter")
	}
	rest := strings.TrimPrefix(text, "---\n")
	parts := strings.SplitN(rest, "\n---", 2)
	if len(parts) != 2 {
		return Note{}, fmt.Errorf("note frontmatter 未闭合")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(parts[0], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = unquoteFrontmatter(strings.TrimSpace(value))
	}
	createdAt, _ := time.Parse(time.RFC3339, fields["created_at"])
	updatedAt, _ := time.Parse(time.RFC3339, fields["updated_at"])
	note := Note{
		ID:         fields["id"],
		Title:      fields["title"],
		Type:       NoteType(fields["type"]),
		Scope:      Scope(fields["scope"]),
		Source:     fields["source"],
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
		Supersedes: parseList(fields["supersedes"]),
		Body:       strings.TrimSpace(strings.TrimPrefix(parts[1], "\n")),
	}
	if strings.TrimSpace(note.ID) == "" {
		return Note{}, fmt.Errorf("note 缺少 id")
	}
	return sanitizeNote(note), nil
}

func sanitizeNote(note Note) Note {
	note.ID = safeID(note.ID)
	note.Title = strings.TrimSpace(redact.Text(note.Title))
	note.Source = strings.TrimSpace(redact.Text(note.Source))
	note.Body = strings.TrimSpace(redact.Text(note.Body))
	items := make([]string, 0, len(note.Supersedes))
	for _, item := range note.Supersedes {
		if cleaned := safeID(item); cleaned != "" {
			items = append(items, cleaned)
		}
	}
	note.Supersedes = items
	return note
}

func noteFrontmatterLines(note Note) []string {
	lines := []string{
		"id: " + quoteFrontmatter(note.ID),
		"title: " + quoteFrontmatter(note.Title),
		"type: " + quoteFrontmatter(string(note.Type)),
		"scope: " + quoteFrontmatter(string(note.Scope)),
		"source: " + quoteFrontmatter(note.Source),
		"created_at: " + quoteFrontmatter(note.CreatedAt.UTC().Format(time.RFC3339)),
		"updated_at: " + quoteFrontmatter(note.UpdatedAt.UTC().Format(time.RFC3339)),
	}
	if len(note.Supersedes) > 0 {
		lines = append(lines, "supersedes: "+quoteFrontmatter(strings.Join(note.Supersedes, ",")))
	}
	sort.Strings(lines)
	return lines
}

func quoteFrontmatter(value string) string {
	return strconv.Quote(strings.TrimSpace(value))
}

func unquoteFrontmatter(value string) string {
	unquoted, err := strconv.Unquote(value)
	if err != nil {
		return strings.Trim(value, "\"'")
	}
	return unquoted
}

func parseList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if cleaned := safeID(part); cleaned != "" {
			items = append(items, cleaned)
		}
	}
	return items
}

func noteID(noteType NoteType, scope Scope, title string, body string, now time.Time) string {
	seed := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", noteType, scope, title, body, now.UTC().Format(time.RFC3339Nano))
	return hashText(seed)[:16]
}

func safeID(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}
