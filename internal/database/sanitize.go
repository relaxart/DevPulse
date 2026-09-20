package database

import "regexp"

// credentialPattern matches user:password@ segments of connection strings.
var credentialPattern = regexp.MustCompile(`(?i)(postgres(?:ql)?://)[^:@/\s]+:[^@/\s]+@`)

// sanitize removes embedded credentials so connection errors are safe to log.
func sanitize(s string) string {
	return credentialPattern.ReplaceAllString(s, "${1}[redacted]@")
}
