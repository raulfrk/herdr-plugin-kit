package diagnostics

import (
	"regexp"
	"sort"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/snapshot"
)

var sensitiveKeyPatterns = compileSensitiveKeyPatterns()

func redactValue(value any) any {
	return redactValueWithKeys(value, make(map[string]redactionKey))
}

type redactionKey struct {
	sensitive bool
	redacted  string
}

func redactValueWithKeys(value any, decisions map[string]redactionKey) any {
	switch value := value.(type) {
	case string:
		return snapshot.Redact(value)
	case []any:
		result := make([]any, len(value))
		for index := range value {
			result[index] = redactValueWithKeys(value[index], decisions)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(value))
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := value[key]
			decision, ok := decisions[key]
			if !ok {
				decision.sensitive = sensitiveDetailKey(key)
				if !decision.sensitive {
					decision.redacted = snapshot.Redact(key)
				}
				decisions[key] = decision
			}
			if decision.sensitive {
				result[key] = snapshot.Replacement
				continue
			}
			result[decision.redacted] = redactValueWithKeys(item, decisions)
		}
		return result
	default:
		return value
	}
}

func sensitiveDetailKey(key string) bool {
	key = strings.Trim(strings.ToLower(key), `"'`)
	key = key[strings.LastIndexByte(key, '.')+1:]
	for _, pattern := range sensitiveKeyPatterns {
		if pattern.MatchString(key) {
			return true
		}
	}
	return false
}

func compileSensitiveKeyPatterns() []*regexp.Regexp {
	patterns := snapshot.SensitiveKeyPatterns()
	compiled := make([]*regexp.Regexp, len(patterns))
	for index, pattern := range patterns {
		compiled[index] = regexp.MustCompile(pattern)
	}
	return compiled
}
