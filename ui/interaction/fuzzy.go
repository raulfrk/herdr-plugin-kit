package interaction

import (
	"sort"
	"strings"
	"unicode"
)

// Candidate is one dependency-free fuzzy-ranking input.
type Candidate struct {
	Key  string
	Text string
}

// MatchResult describes a subsequence match. Positions are rune indexes in Text.
type MatchResult struct {
	Candidate Candidate
	Score     int
	Positions []int
}

// Match performs a case-insensitive Unicode subsequence match.
func Match(query, text string) (MatchResult, bool) {
	queryRunes := []rune(strings.ToLower(query))
	textRunes := []rune(strings.ToLower(text))
	match := MatchResult{Positions: make([]int, 0, len(queryRunes))}
	if len(queryRunes) == 0 {
		return match, true
	}

	next := 0
	for _, queryRune := range queryRunes {
		found := -1
		for index := next; index < len(textRunes); index++ {
			if textRunes[index] == queryRune {
				found = index
				break
			}
		}
		if found < 0 {
			return MatchResult{}, false
		}
		match.Positions = append(match.Positions, found)
		next = found + 1
	}

	match.Score = 1000 - match.Positions[0]*4 - (len(textRunes) - len(queryRunes))
	for index, position := range match.Positions {
		if position == 0 || !unicode.IsLetter(textRunes[position-1]) && !unicode.IsNumber(textRunes[position-1]) {
			match.Score += 12
		}
		if index > 0 && position == match.Positions[index-1]+1 {
			match.Score += 20
		}
	}
	if strings.EqualFold(query, text) {
		match.Score += 100
	}
	return match, true
}

// Rank returns matching candidates in deterministic best-first order. Ties
// preserve input order, so callers control the final stable ordering.
func Rank(query string, candidates []Candidate) []MatchResult {
	matches := make([]MatchResult, 0, len(candidates))
	for _, candidate := range candidates {
		match, ok := Match(query, candidate.Text)
		if !ok {
			continue
		}
		match.Candidate = candidate
		matches = append(matches, match)
	}
	sort.SliceStable(matches, func(left, right int) bool { return matches[left].Score > matches[right].Score })
	return matches
}
