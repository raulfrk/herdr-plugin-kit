package diagnostics

import (
	"regexp"
	"sort"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/snapshot"
)

var sensitiveKeyPatterns = compileSensitiveKeyPatterns()

func redactValue(value any) any {
	switch value := value.(type) {
	case string:
		return snapshot.Redact(value)
	case []any:
		result := make([]any, len(value))
		for index := range value {
			result[index] = redactValue(value[index])
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
			if sensitiveDetailKey(key) {
				result[key] = snapshot.Replacement
				continue
			}
			result[snapshot.Redact(key)] = redactValue(item)
		}
		return result
	default:
		return value
	}
}

func sensitiveDetailKey(key string) bool {
	key = strings.Trim(strings.ToLower(key), `"'`)
	if index := strings.LastIndexByte(key, '.'); index != -1 {
		key = key[index+1:]
	}
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
