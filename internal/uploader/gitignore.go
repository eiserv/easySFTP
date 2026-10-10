package uploader

import (
	"fmt"
	"strings"
)

// The exclude patterns are documented as gitignore syntax
// (docs/configuration.md, action.yml, README.md), and issue #314 is what
// happened to the last attempt at honoring that promise: the previous
// matcher translated each line into a regular expression with string
// replacements, escaped only ".", "*" and "?", and passed every other
// metacharacter to the regexp engine verbatim. "?", "[!...]", "+", "(",
// "|", "$" and "^" did not mean what gitignore says they mean, a pattern
// whose regex did not compile was dropped without a word, and a pattern
// with a slash in the middle was matched anywhere in the tree instead of
// at the root. This file follows the gitignore rules themselves instead:
// segment-by-segment matching, anchored when the pattern contains a
// slash anywhere but at the end, trailing-slash directory patterns, "**"
// segments, backslash escapes, and character classes including the
// "[!...]" negated form. A path is decided
// the way git decides it: by the last pattern that matches the path
// itself, or, when none does, by an excluded ancestor directory
// ("dist" excludes "dist/a.js" and "x/dist/a.js" the same way
// "dist/" does, because a git walk never descends into an
// ignored directory).
//
// One divergence from git is deliberate, pinned by
// TestIgnoreNegationReincludesBelowIgnoredDirectory and documented in
// docs/configuration.md: a "!..." re-include is honored below an excluded
// directory (git stops at the excluded directory and never descends, so
// it cannot re-include anything below it). easySFTP keeps the re-include
// because pruning is an optimization here, not the semantics, and the
// pruning is disabled automatically when a "!" line is present.

// gitignorePattern is one parsed exclude line.
type gitignorePattern struct {
	Line     string // the line as written, for verbose logging
	negate   bool   // a "!..." re-include
	dirOnly  bool   // trailing slash: matches directories only
	anchored bool   // slash anywhere but at the end: match from the root
	segments []string
}

// gitignoreMatcher answers one question for the planner: does this path
// fall to an exclude pattern? Patterns are evaluated in order and the
// last one that matches wins, exactly as git evaluates them.
type gitignoreMatcher struct {
	patterns []gitignorePattern
}

// compileGitignore parses the exclude lines. A line that is a comment,
// blank, or only a negation prefix is skipped the way git skips it. A line
// with a syntax error (an unterminated character class, a trailing lone
// backslash) is an error: silently dropping an exclude is the failure
// mode that publishes a file the user asked to keep out (issue #314).
func compileGitignore(lines []string) (*gitignoreMatcher, error) {
	m := &gitignoreMatcher{}
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		// A line starting with "#" is a comment; an escaped "#" is
		// literal, and the escape is dropped below by the segment
		// parser matching it against itself.
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Trailing spaces are ignored unless escaped with a backslash;
		// tabs are name characters and are never trimmed.
		line = trimRightUnescapedSpace(line)
		if line == "" {
			continue
		}
		p := gitignorePattern{Line: raw}
		body := line
		if strings.HasPrefix(body, "!") {
			p.negate = true
			body = body[1:]
		} else if strings.HasPrefix(body, "\\!") {
			// The escape is dropped, "!" is literal.
			body = body[1:]
		}
		if body == "" {
			// "!" alone re-includes nothing, same as git.
			continue
		}
		// A slash anywhere except at the end anchors the pattern at the
		// root; a leading slash does the same and is not part of any
		// segment, so it is dropped once it has anchored the pattern.
		trimmed := strings.TrimSuffix(body, "/")
		if strings.HasPrefix(body, "/") || strings.Contains(trimmed, "/") {
			p.anchored = true
		}
		if strings.HasPrefix(body, "/") {
			body = body[1:]
			trimmed = strings.TrimSuffix(body, "/")
		}
		if strings.HasSuffix(body, "/") {
			p.dirOnly = true
			body = trimmed
		}
		segments, err := parseGitignoreSegments(body, i+1, raw)
		if err != nil {
			return nil, err
		}
		p.segments = segments
		m.patterns = append(m.patterns, p)
	}
	return m, nil
}

// trimRightUnescapedSpace strips the trailing spaces gitignore trims:
// unescaped ones only. An escaped space (a backslash before it) is a
// literal space in a name, and a tab is a name character git never
// trims at all.
func trimRightUnescapedSpace(line string) string {
	end := len(line)
	for end > 0 && line[end-1] == ' ' {
		// Count the backslashes immediately before the space: an odd
		// run escapes it and ends the trimming.
		bs := 0
		for i := end - 2; i >= 0 && line[i] == BS; i-- {
			bs++
		}
		if bs%2 == 1 {
			break
		}
		end--
	}
	return line[:end]
}

// matchesHow reports whether path is excluded, and the pattern that
// decided it. isDir marks a directory path; the planner walks
// directories separately and passes the fact here, because a
// trailing-slash pattern only ever matches a directory. Paths are
// slash-separated and relative to the deployment local root, which is
// exactly the form the planner works in.
//
// The evaluation mirrors git. The last pattern matching the path
// itself decides it, and a "!" line that matches re-includes. When no
// pattern matches the path itself, an excluded ancestor directory
// excludes everything below it, the way a git walk never descends into
// an ignored directory. The one deliberate difference is pinned by
// TestIgnoreNegationReincludesBelowIgnoredDirectory and documented in
// docs/configuration.md: the leaf verdict wins even below an excluded
// directory, so a "!" line can re-include a file git would never
// reach, because the planner disables pruning whenever a "!" line is
// present and walks the whole tree.
func (m *gitignoreMatcher) matchesHow(path string, isDir bool) (bool, *gitignorePattern) {
	if path == "." {
		return false, nil
	}
	segs := strings.Split(path, "/")
	if excluded, pat, decided := m.lastMatchHow(segs, isDir); decided {
		return excluded, pat
	}
	for k := len(segs) - 1; k >= 1; k-- {
		// The ancestors of a path are directories by definition.
		if excluded, pat, decided := m.lastMatchHow(segs[:k], true); decided && excluded {
			return true, pat
		}
	}
	return false, nil
}

// lastMatchHow runs the patterns, last first, against one path and
// reports the verdict of the last one that matches. decided is false
// when no pattern matched the path at all.
func (m *gitignoreMatcher) lastMatchHow(segs []string, isDir bool) (excluded bool, pat *gitignorePattern, decided bool) {
	for i := len(m.patterns) - 1; i >= 0; i-- {
		p := &m.patterns[i]
		if p.matchesPath(segs, isDir) {
			return !p.negate, p, true
		}
	}
	return false, nil, false
}

// matchesPath applies one pattern to one path: an anchored pattern
// matches the whole path, an unanchored one matches its last segment
// (a slash in the middle anchors a pattern, so an unanchored pattern is
// always a single segment), and a trailing-slash pattern matches
// directories only.
func (p *gitignorePattern) matchesPath(segs []string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if p.anchored {
		return matchSegments(p.segments, segs)
	}
	if len(segs) == 0 {
		return false
	}
	return matchSegments(p.segments, segs[len(segs)-1:])
}

// matchSegments matches pattern segments against path segments. A
// pattern that ends before the path does names everything below it
// ("dist" excludes "dist/a.js", "docs/**/notes" excludes everything
// below a directory it matches), which is how gitignore containment
// works: a pattern that matches a directory matches the whole subtree.
func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return true
	}
	if !containsStarStar(pat) {
		return matchSegmentsPlain(pat, segs)
	}
	// A pattern with "**" segments is matched through a memo table keyed
	// by (pattern index, path index), so a hostile pattern with many
	// separated "**" segments cannot walk an exponential number of
	// states (the lesson of the shared-anchor walk in issue #290).
	memo := make(map[[2]int]bool)
	return matchSegmentsAt(pat, 0, segs, 0, memo)
}

// matchSegmentsPlain is matchSegments for patterns without "**": a
// plain recursion, one step per segment, no allocation.
func matchSegmentsPlain(pat, segs []string) bool {
	if len(pat) == 0 {
		return true
	}
	if len(segs) == 0 {
		return false
	}
	if !matchSegment(pat[0], segs[0]) {
		return false
	}
	return matchSegmentsPlain(pat[1:], segs[1:])
}

// containsStarStar reports whether any pattern segment is "**".
func containsStarStar(pat []string) bool {
	for _, s := range pat {
		if s == "**" {
			return true
		}
	}
	return false
}

// matchSegmentsAt is the memoized form of the "**" recursion. A
// non-trailing "**" swallows zero or more path segments; a trailing
// one must swallow at least one, so "foo/**" names everything below
// foo but not foo itself, the way git reads it.
func matchSegmentsAt(pat []string, pi int, segs []string, si int, memo map[[2]int]bool) bool {
	key := [2]int{pi, si}
	if v, ok := memo[key]; ok {
		return v
	}
	res := false
	defer func() { memo[key] = res }()
	if pi == len(pat) {
		// The pattern named every segment it could: this path is below
		// what it describes.
		res = true
		return true
	}
	if pat[pi] == "**" {
		if pi == len(pat)-1 {
			res = si < len(segs)
			return res
		}
		if matchSegmentsAt(pat, pi+1, segs, si, memo) {
			res = true
			return true
		}
		for j := si + 1; j <= len(segs); j++ {
			if matchSegmentsAt(pat, pi+1, segs, j, memo) {
				res = true
				return true
			}
		}
		return false
	}
	if si == len(segs) {
		return false
	}
	if !matchSegment(pat[pi], segs[si]) {
		return false
	}
	res = matchSegmentsAt(pat, pi+1, segs, si+1, memo)
	return res
}

// matchSegment matches one pattern segment against one path segment,
// honoring "*", "?" and "[...]" classes including the "[!...]" negated
// form, with backslash escapes honored the way git fnmatch honors them.
func matchSegment(pat, str string) bool {
	// Fast path for segments without metacharacters.
	if !strings.ContainsAny(pat, "*?[") && strings.IndexByte(pat, BS) < 0 {
		return pat == str
	}
	return matchSegmentHere(pat, str)
}

func matchSegmentHere(pat, str string) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case BS:
			// Backslash: escape the next character, literal when trailing.
			if len(pat) == 1 {
				return str == string(rune(BS))
			}
			if len(str) == 0 || str[0] != pat[1] {
				return false
			}
			pat, str = pat[2:], str[1:]
		case '?':
			if len(str) == 0 {
				return false
			}
			pat, str = pat[1:], str[1:]
		case '*':
			// "*" matches any run of characters within the segment; the
			// segment split already removed the slashes it must not
			// cross.
			for rest := 0; rest <= len(str); rest++ {
				if matchSegmentHere(pat[1:], str[rest:]) {
					return true
				}
			}
			return false
		case '[':
			// Character class: "[abc]", "[a-z]", "[!abc]". A "]" first
			// inside the class is literal, the fnmatch rule.
			body, rest, ok := splitClass(pat)
			if !ok {
				// Unterminated: parse time rejects it; a literal "[".
				if len(str) == 1 && str[0] == '[' {
					pat, str = pat[1:], str[1:]
					continue
				}
				return false
			}
			negate := false
			if strings.HasPrefix(body, "!") {
				negate = true
				body = body[1:]
			}
			if len(str) == 0 {
				return false
			}
			in := classContains(body, str[0])
			if in == negate {
				return false
			}
			pat, str = rest, str[1:]
		default:
			if len(str) == 0 || str[0] != pat[0] {
				return false
			}
			pat, str = pat[1:], str[1:]
		}
	}
	return len(str) == 0
}

// splitClass takes a pattern starting at "[" and returns the inside of
// the class and the pattern after its closing "]". ok is false when the
// class never closes.
func splitClass(pat string) (body, rest string, ok bool) {
	// pat[0] is the "[" itself.
	i := 1
	if i < len(pat) && pat[i] == ']' {
		i++
	}
	for i < len(pat) {
		switch pat[i] {
		case BS:
			i += 2
			continue
		case ']':
			return pat[1:i], pat[i+1:], true
		}
		i++
	}
	return "", "", false
}

// classContains evaluates the inside of one character class against one
// char. Ranges "a-z" and backslash escapes are honored; a "-" at either
// end of the class is a literal dash.
func classContains(cls string, c byte) bool {
	for i := 0; i < len(cls); i++ {
		if cls[i] == BS && i+1 < len(cls) {
			if c == cls[i+1] {
				return true
			}
			i++
			continue
		}
		if i+2 < len(cls) && cls[i+1] == '-' {
			lo, hi := cls[i], cls[i+2]
			if c >= lo && c <= hi {
				return true
			}
			i += 2
			continue
		}
		if cls[i] == c {
			return true
		}
	}
	return false
}

// BS is the backslash byte, the escape character of gitignore patterns.
// It is a constant so the matcher reads as grammar rules instead of a
// run of escapes.
const BS = 92

// parseGitignoreSegments splits a pattern body into slash-separated
// segments and rejects the syntax that would otherwise silently mean
// something else. A character class that never closes is the case worth
// failing on: the pattern would match nothing and the user would not
// learn why.
func parseGitignoreSegments(body string, lineNo int, raw string) ([]string, error) {
	if i := strings.IndexByte(body, '['); i >= 0 {
		if !classClosed(body[i:]) {
			return nil, fmt.Errorf("exclude pattern %d: %q has an unterminated character class", lineNo, raw)
		}
	}
	if len(body) > 0 && body[len(body)-1] == BS {
		// A lone trailing backslash escapes nothing; refusing it keeps the
		// intent visible instead of matching a pattern the user cannot
		// have meant.
		return nil, fmt.Errorf("exclude pattern %d: %q ends with a lone backslash", lineNo, raw)
	}
	if strings.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("exclude pattern %d: %q contains a NUL byte", lineNo, raw)
	}
	segments := strings.Split(body, "/")
	// A run of "**" segments is one "**": zero-or-more twice is
	// zero-or-more. Collapsing the run keeps pathological patterns
	// cheap and the memo table small.
	out := segments[:0]
	for _, seg := range segments {
		if seg == "**" && len(out) > 0 && out[len(out)-1] == "**" {
			continue
		}
		out = append(out, seg)
	}
	return out, nil
}

// classClosed reports whether the class starting at rest[0] closes.
// Escapes are honored: an escaped "]" does not close the class.
func classClosed(rest string) bool {
	i := 1
	if i < len(rest) && rest[i] == ']' {
		i++
	}
	for i < len(rest) {
		switch rest[i] {
		case BS:
			i += 2
			continue
		case ']':
			return true
		}
		i++
	}
	return false
}
