package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

const (
	registeredPrefix = "mcp__"
	maxNamePartLen   = 48
	maxToolNameLen   = 128
)

type ToolIdentity struct {
	RegisteredName string
	ServerName     string
	RemoteToolName string
}

func RegisteredToolName(serverName string, remoteToolName string, used map[string]bool) ToolIdentity {
	serverPart := sanitizeNamePart(serverName, "server")
	toolPart := sanitizeNamePart(remoteToolName, "tool")
	name := registeredPrefix + serverPart + "__" + toolPart
	if len(name) > maxToolNameLen {
		hash := shortHash(serverName + "\x00" + remoteToolName)
		budget := maxToolNameLen - len(registeredPrefix) - len("__") - len(hash) - 1
		serverBudget := budget / 2
		toolBudget := budget - serverBudget
		serverPart = trimWithHash(serverPart, serverBudget, shortHash(serverName))
		toolPart = trimWithHash(toolPart, toolBudget, shortHash(remoteToolName))
		name = registeredPrefix + serverPart + "__" + toolPart + "_" + hash
	}
	if used != nil {
		base := name
		for used[name] {
			suffix := "_" + shortHash(serverName+"\x00"+remoteToolName+"\x00"+name)
			name = trimTo(maxToolNameLen-len(suffix), base) + suffix
		}
		used[name] = true
	}
	return ToolIdentity{RegisteredName: name, ServerName: serverName, RemoteToolName: remoteToolName}
}

func SanitizeMetadata(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	var builder strings.Builder
	lastSpace := false
	for _, r := range value {
		if builder.Len() >= limit {
			break
		}
		if unicode.IsControl(r) {
			if !lastSpace {
				builder.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		builder.WriteRune(r)
		lastSpace = unicode.IsSpace(r)
	}
	return strings.TrimSpace(builder.String())
}

func sanitizeNamePart(value string, fallback string) string {
	var builder strings.Builder
	lastUnderscore := false
	for _, r := range value {
		valid := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if valid {
			builder.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
	}
	part := strings.Trim(builder.String(), "_")
	if part == "" {
		part = fallback + "_" + shortHash(value)
	}
	if len(part) > maxNamePartLen {
		part = trimWithHash(part, maxNamePartLen-len(shortHash(value))-1, shortHash(value))
	}
	return part
}

func trimWithHash(value string, prefixLen int, hash string) string {
	return trimTo(prefixLen, value) + "_" + hash
}

func trimTo(limit int, value string) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	return strings.TrimRight(value[:limit], "_")
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

func validRegisteredToolName(value string) bool {
	if !strings.HasPrefix(value, registeredPrefix) || len(value) > maxToolNameLen {
		return false
	}
	for _, r := range value {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return len(value) > len(registeredPrefix)
}

func validServerConfigDigest(digest [32]byte) bool {
	return digest != [32]byte{}
}
