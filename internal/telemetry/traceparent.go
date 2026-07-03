package telemetry

import "strings"

// ParseTraceparent extracts the trace id and parent span id from a W3C
// trace-context traceparent header of the form
//
//	version "-" trace-id "-" parent-id "-" trace-flags
//	e.g. 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
//
// It accepts version "00" (the only version defined today) with a 32-hex-char
// trace id and a 16-hex-char span id, both of which must be non-zero per the
// spec. ok is false for any malformed, absent, or all-zero value, in which case
// the returned ids are empty. The returned ids are lowercased.
func ParseTraceparent(header string) (traceID, spanID string, ok bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", "", false
	}
	parts := strings.Split(header, "-")
	if len(parts) < 4 {
		return "", "", false
	}
	version, tid, sid := parts[0], strings.ToLower(parts[1]), strings.ToLower(parts[2])
	if version != "00" {
		return "", "", false
	}
	if len(tid) != 32 || !isHex(tid) || isAllZero(tid) {
		return "", "", false
	}
	if len(sid) != 16 || !isHex(sid) || isAllZero(sid) {
		return "", "", false
	}
	return tid, sid, true
}

// isHex reports whether s consists solely of hexadecimal digits.
func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// isAllZero reports whether s is entirely '0' characters (an invalid id).
func isAllZero(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}
