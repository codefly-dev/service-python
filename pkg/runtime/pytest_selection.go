package runtime

// ARCHITECTURE: Runtime filters are language-neutral regular expressions;
// pytest's `-k` option is a different Boolean substring language. This file is
// the Python-agent boundary that performs only lossless, bounded translation.
// Mind never constructs or interprets pytest command-line expressions.

import (
	"fmt"
	"regexp/syntax"
	"strings"
)

const maxPytestFilterAlternatives = 128

// normalizePytestNameFilters translates Codefly's language-neutral regular
// expression filters into the finite literal alternatives pytest's `-k`
// expression can represent. Pytest does not accept regular expressions: a
// request such as `test_one|test_two` used to be forwarded verbatim, rejected
// by pytest's parser, and then misreported as an environment outage. The
// Python plugin owns this translation so Mind remains runner-blind.
//
// Infinite or context-sensitive expressions fail before project execution
// instead of silently widening the selection. Callers that need an exact
// parameterized identity use the typed TestSelection contract.
func normalizePytestNameFilters(patterns []string) ([]string, error) {
	filters := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		expression, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			return nil, fmt.Errorf("invalid Python test name filter %q: %w", pattern, err)
		}
		alternatives, ok := finiteRegexLiterals(expression.Simplify(), maxPytestFilterAlternatives)
		if !ok {
			return nil, fmt.Errorf("Python test name filter %q cannot be represented by pytest name selection; use finite literal alternatives or a typed TestSelection", pattern)
		}
		for _, alternative := range alternatives {
			alternative = strings.TrimSpace(alternative)
			if alternative == "" {
				return nil, fmt.Errorf("Python test name filter %q contains an empty alternative that would select the whole suite", pattern)
			}
			if _, exists := seen[alternative]; exists {
				continue
			}
			seen[alternative] = struct{}{}
			filters = append(filters, alternative)
		}
	}
	return filters, nil
}

// finiteRegexLiterals enumerates only a bounded, finite regular language.
// Literals, concatenation, alternation, captures, and small character classes
// are losslessly representable as pytest name substrings. Repetition,
// wildcard, boundary, and empty-match operators are rejected because mapping
// them onto `-k` would change the requested scope.
func finiteRegexLiterals(expression *syntax.Regexp, limit int) ([]string, bool) {
	if expression == nil || limit <= 0 {
		return nil, false
	}
	switch expression.Op {
	case syntax.OpLiteral:
		return []string{string(expression.Rune)}, true
	case syntax.OpCapture:
		return finiteRegexLiterals(expression.Sub[0], limit)
	case syntax.OpAlternate:
		var result []string
		for _, sub := range expression.Sub {
			values, ok := finiteRegexLiterals(sub, limit-len(result))
			if !ok || len(result)+len(values) > limit {
				return nil, false
			}
			result = append(result, values...)
		}
		return result, true
	case syntax.OpConcat:
		result := []string{""}
		for _, sub := range expression.Sub {
			values, ok := finiteRegexLiterals(sub, limit)
			if !ok || len(result)*len(values) > limit {
				return nil, false
			}
			product := make([]string, 0, len(result)*len(values))
			for _, prefix := range result {
				for _, suffix := range values {
					product = append(product, prefix+suffix)
				}
			}
			result = product
		}
		return result, true
	case syntax.OpCharClass:
		var result []string
		for index := 0; index+1 < len(expression.Rune); index += 2 {
			first, last := expression.Rune[index], expression.Rune[index+1]
			if last-first+1 > rune(limit-len(result)) {
				return nil, false
			}
			for value := first; value <= last; value++ {
				result = append(result, string(value))
			}
		}
		return result, len(result) > 0 && len(result) <= limit
	default:
		return nil, false
	}
}
