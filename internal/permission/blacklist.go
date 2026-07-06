package permission

import "regexp"

type blacklistRule struct {
	pattern *regexp.Regexp
	reason  string
}

var bashBlacklist = []blacklistRule{
	{regexp.MustCompile(`(?i)(^|\s)rm\s+(-[^\s]*[rf][^\s]*|-[^\s]*r[^\s]*\s+-[^\s]*f[^\s]*).*\s(/|~|/System|/usr|/bin|/sbin|/etc)(\s|$)`), "destructive delete"},
	{regexp.MustCompile(`(?i)(^|\s)(mkfs|diskutil\s+eraseDisk|fdisk|parted)(\s|$)`), "disk destructive command"},
	{regexp.MustCompile(`(?i)(^|\s)dd\s+.*\bof=/dev/`), "raw device write"},
	{regexp.MustCompile(`(?i)(^|\s)(chmod|chown)\s+.*\s(/|/System|/usr|/bin|/sbin|/etc)(\s|$)`), "system permission change"},
	{regexp.MustCompile(`(?i)(^|\s)(shutdown|reboot|halt)(\s|$)`), "system shutdown"},
	{regexp.MustCompile(`:\s*\(\)\s*\{\s*:\s*\|\s*:\s*&\s*}\s*;\s*:`), "fork bomb"},
	{regexp.MustCompile(`(?i)(^|\s)(sudo|su)(\s|$)`), "privilege escalation"},
	{regexp.MustCompile(`(?i)(^|\s).*>\s*(/System|/usr/bin|/bin|/sbin|/etc)(/|\s|$)`), "write system path"},
	{regexp.MustCompile(`(?i)(^|\s)git\s+reset\s+--hard(\s|$)`), "destructive git history"},
	{regexp.MustCompile(`(?i)(^|\s)git\s+push\s+(-[^\s]*f|--force)(\s|$)`), "destructive git push"},
	{regexp.MustCompile(`(?i)(^|\s)git\s+clean\s+(-[^\s]*f|.*\s-f)(\s|$)`), "destructive git clean"},
}

func CheckBashBlacklist(command string) (string, bool) {
	for _, rule := range bashBlacklist {
		if rule.pattern.MatchString(command) {
			return rule.reason, true
		}
	}
	return "", false
}
