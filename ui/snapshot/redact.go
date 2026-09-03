// Package snapshot contains deterministic snapshot and diagnostic helpers.
package snapshot

import (
	"regexp"
	"strings"
)

const Replacement = "<redacted>"

var sensitiveKeyPatternText = []string{
	`(?i)^(?:[a-z0-9]+[_-])*api[_-]?key$`,
	`(?i)^(?:[a-z0-9]+[_-])*(?:access[_-]?|refresh[_-]?)?token$`,
	`(?i)^(?:[a-z0-9]+[_-])*(?:pass(?:word|wd)?|secret)$`,
	`(?i)^(?:authorization|cookie|(?:[a-z0-9]+[_-])*private[_-]?key)$`,
}

var (
	sensitiveKeyPatterns = compilePatterns(sensitiveKeyPatternText)
	quotedValue          = `"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`
	assignment           = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(["']?)([A-Za-z][A-Za-z0-9_.-]*)(["']?[\t ]*[:=][\t ]*)((?:` + quotedValue + `|<redacted>|\\.|[^,;\t \}\]\)"'&|()<>])+)`)
	flagName             = regexp.MustCompile(`--([A-Za-z][A-Za-z0-9_-]*)`)
	sensitiveHeader      = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])((authorization|cookie)["']?[\t ]*:[\t ]*)`)
	bearerToken          = regexp.MustCompile(`(?i)(bearer[\t ]+)([A-Za-z0-9._~+/\-]+=*|<redacted>[A-Za-z0-9._~+/\-]*=*)`)
)

// SensitiveKeyPatterns returns an immutable copy of the explicit regex policy.
func SensitiveKeyPatterns() []string { return append([]string(nil), sensitiveKeyPatternText...) }

// Redact replaces values assigned to explicit sensitive keys. It handles
// common TOML/env/JSON-like lines, CLI flags, and bearer credentials.
func Redact(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = redactHeaders(line)
		line = redactBearer(line)
		line = redactFlags(line)
		lines[i] = redactMatches(line, assignment)
	}
	return strings.Join(lines, "\n")
}

func redactHeaders(line string) string {
	var output strings.Builder
	for cursor := 0; cursor < len(line); {
		match := sensitiveHeader.FindStringSubmatchIndex(line[cursor:])
		if match == nil {
			output.WriteString(line[cursor:])
			break
		}
		end := cursor + match[1]
		key := line[cursor+match[6] : cursor+match[7]]
		boundary := line[cursor+match[2] : cursor+match[3]]
		headerPrefix := line[cursor+match[4] : cursor+match[5]]
		output.WriteString(line[cursor:end])
		valueStart := end
		quote := byte(0)
		structured := false
		if valueStart < len(line) && (line[valueStart] == '\'' || line[valueStart] == '"') {
			quote = line[valueStart]
			output.WriteByte(quote)
			valueStart++
		} else if boundary == "'" || boundary == `"` {
			if strings.Contains(headerPrefix, boundary) {
				structured = true
			} else {
				quote = boundary[0]
			}
		}
		output.WriteString(Replacement)
		if strings.HasPrefix(line[valueStart:], Replacement) {
			valueStart += len(Replacement)
		}
		cursor = headerValueEnd(line, valueStart, quote, key, structured)
	}
	return output.String()
}

func headerValueEnd(line string, start int, quote byte, key string, structured bool) int {
	for index := start; index < len(line); index++ {
		if line[index] == '\\' && index+1 < len(line) {
			index++
			continue
		}
		if quote != 0 {
			if line[index] == quote {
				return index
			}
			continue
		}
		if structured && (line[index] == ',' || line[index] == '}' || line[index] == ']') {
			return index
		}
		if headerBoundary(line, index, key) {
			return index
		}
	}
	return len(line)
}

func headerBoundary(line string, index int, key string) bool {
	value := line[index]
	if value == ',' && strings.EqualFold(key, "authorization") {
		return true
	}
	if (value == ';' || value == ',') && sensitiveHeader.MatchString(strings.TrimLeft(line[index+1:], " \t")) {
		return true
	}
	if !isShellBoundary(value) {
		return false
	}
	if strings.EqualFold(key, "authorization") || value != ';' {
		return true
	}
	rest := strings.TrimLeft(line[index+1:], " \t")
	wordEnd := strings.IndexAny(rest, " \t;")
	if wordEnd < 0 {
		wordEnd = len(rest)
	}
	return !strings.Contains(rest[:wordEnd], "=")
}

func redactBearer(line string) string {
	var output strings.Builder
	for cursor := 0; cursor < len(line); {
		match := bearerToken.FindStringSubmatchIndex(line[cursor:])
		if match == nil {
			output.WriteString(line[cursor:])
			break
		}
		start, end := cursor+match[0], cursor+match[1]
		if start > 0 && isBearerWord(line[start-1]) {
			output.WriteString(line[cursor : start+1])
			cursor = start + 1
			continue
		}
		if end < len(line) && line[end] == '%' {
			output.WriteString(line[cursor:end])
			cursor = end
			continue
		}
		output.WriteString(line[cursor : cursor+match[3]])
		output.WriteString(Replacement)
		cursor = end
	}
	return output.String()
}

func isBearerWord(value byte) bool {
	return value == '-' || value == '_' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func redactMatches(line string, expression *regexp.Regexp) string {
	return expression.ReplaceAllStringFunc(line, func(match string) string {
		parts := expression.FindStringSubmatch(match)
		if len(parts) != 6 || !sensitive(parts[3]) {
			return match
		}
		if (strings.EqualFold(parts[3], "authorization") || strings.EqualFold(parts[3], "cookie")) && strings.Contains(parts[4], ":") {
			return match
		}
		return parts[1] + parts[2] + parts[3] + parts[4] + Replacement
	})
}

func redactFlags(line string) string {
	var output strings.Builder
	for cursor := 0; cursor < len(line); {
		match := flagName.FindStringSubmatchIndex(line[cursor:])
		if match == nil {
			output.WriteString(line[cursor:])
			break
		}
		start, end := cursor+match[0], cursor+match[1]
		if start > 0 && isBearerWord(line[start-1]) {
			output.WriteString(line[cursor : start+1])
			cursor = start + 1
			continue
		}
		key := line[cursor+match[2] : cursor+match[3]]
		output.WriteString(line[cursor:end])
		if !sensitive(key) {
			cursor = end
			continue
		}
		valueStart := end
		if valueStart < len(line) && line[valueStart] == '=' {
			output.WriteByte('=')
			valueStart++
		} else {
			spaceStart := valueStart
			for valueStart < len(line) && (line[valueStart] == ' ' || line[valueStart] == '\t') {
				valueStart++
			}
			if valueStart == spaceStart || strings.HasPrefix(line[valueStart:], "--") {
				cursor = end
				continue
			}
			output.WriteString(line[spaceStart:valueStart])
		}
		valueEnd := shellValueEnd(line, valueStart)
		if strings.HasPrefix(line[valueStart:], Replacement) {
			valueEnd = shellValueEnd(line, valueStart+len(Replacement))
		}
		if valueEnd == valueStart {
			cursor = valueStart
			continue
		}
		output.WriteString(Replacement)
		cursor = valueEnd
	}
	return output.String()
}

func shellValueEnd(line string, start int) int {
	index := start
	for index < len(line) {
		if line[index] == ' ' || line[index] == '\t' || isShellBoundary(line[index]) {
			return index
		}
		if line[index] == '\\' && index+1 < len(line) {
			index += 2
			continue
		}
		if line[index] == '\'' || line[index] == '"' {
			quote := line[index]
			index++
			for index < len(line) {
				if line[index] == '\\' && index+1 < len(line) {
					index += 2
					continue
				}
				index++
				if line[index-1] == quote {
					break
				}
			}
			continue
		}
		index++
	}
	return index
}

func isShellBoundary(value byte) bool {
	return strings.ContainsRune(";&|()<>", rune(value))
}

func sensitive(key string) bool {
	key = strings.Trim(strings.ToLower(key), `"'`)
	if index := strings.LastIndexByte(key, '.'); index >= 0 {
		key = key[index+1:]
	}
	for _, pattern := range sensitiveKeyPatterns {
		if pattern != nil && pattern.MatchString(key) {
			return true
		}
	}
	return false
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, pattern := range patterns {
		compiled[i] = regexp.MustCompile(pattern)
	}
	return compiled
}
