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
// docs/configuration.md: a re-include wins below an excluded
// directory, so a "!" line can re-include a file git would never
// reach, because the planner disables pruning whenever a "!" line is
// present and walks the whole tree. But that re-include only wins
// when it is later than every excluded ancestor: an earlier "!"
// re-include does not shield the path from a directory exclude that
// comes after it, the same way the last matching line decides every
// ordinary path. Both verdicts carry their pattern index so the rule
// is a comparison, not a guess.
func (m *gitignoreMatcher) matchesHow(path string, isDir bool) (bool, *gitignorePattern) {
	if path == "." {
		return false, nil
	}
	segs := strings.Split(path, "/")
	leafExcluded, leafPat, leafIdx, leafDecided := m.lastMatchIdx(segs, isDir)
	if leafDecided && leafExcluded {
		// A leaf exclude is final: the path's own last-matching line
		// already decided it, and no ancestor pattern can override
		// that.
		return true, leafPat
	}
	// The ancestors of a path are directories by definition. Every
	// excluded ancestor takes part in the decision, and the last
	// line written wins, so the deciding index is the largest
	// index among them. Stopping at the deepest one, as an
	// earlier round did, let a re-include written before a later,
	// shallower directory exclude fail the run open ("a/b/",
	// "!a/b/x.txt", "a/" uploaded a/b/x.txt where git ignores
	// it), which is the direction issue #314 is about.
	decidingIdx := -1
	var deciding *gitignorePattern
	for k := len(segs) - 1; k >= 1; k-- {
		if excluded, pat, idx, decided := m.lastMatchIdx(segs[:k], true); decided && excluded && idx > decidingIdx {
			decidingIdx, deciding = idx, pat
		}
	}
	if deciding != nil {
		// A leaf re-include beats the excluded ancestors only
		// when it is later than every one of them. A re-include
		// written before any directory exclude leaves that
		// exclude standing, the same way the last matching line
		// decides every ordinary path. The leaf re-include
		// winning below an excluded directory when it is later
		// than every excluded ancestor is the documented
		// divergence.
		if leafDecided && leafIdx > decidingIdx {
			return false, leafPat
		}
		return true, deciding
	}
	if leafDecided {
		return false, leafPat
	}
	return false, nil
}

// lastMatchIdx runs the patterns, last first, against one path and
// reports the verdict of the last one that matches, along with its
// index. decided is false when no pattern matched the path at all.
func (m *gitignoreMatcher) lastMatchIdx(segs []string, isDir bool) (excluded bool, pat *gitignorePattern, idx int, decided bool) {
	for i := len(m.patterns) - 1; i >= 0; i-- {
		p := &m.patterns[i]
		if p.matchesPath(segs, isDir) {
			return !p.negate, p, i, true
		}
	}
	return false, nil, -1, false
}

// matchesPath applies one pattern to one path: an anchored pattern
// matches the whole path, an unanchored one matches its last segment
// (a slash in the middle anchors a pattern, so an unanchored pattern is
// always a single segment), and a trailing-slash pattern matches
// directories only. Both anchored forms, plain and negated, match the
// path exactly. Containment -- "dist" names everything below it -- is
// the ancestor walk's job in matchesHow, never the leaf's: deciding it
// here returned a leaf exclude as final and skipped the ordering a
// later re-include needs, so "/foo/*" + "!/foo/bar" excluded
// foo/bar/x.txt outright (the "exclude everything except directory
// foo/bar" example from git's own gitignore documentation) because
// "/foo/*" contains the leaf through the ancestor foo/bar while the
// "!/foo/bar" that re-includes that directory never got a say. A "!"
// re-include also still means what it always meant: it names the paths
// it re-includes, not a subtree below a directory it happens to match
// ("!/dist" re-includes dist itself, not dist/a.js).
func (p *gitignorePattern) matchesPath(segs []string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if p.anchored {
		// Exact for the negated form (it re-includes the paths it
		// names) and for the plain one alike: the subtree a plain
		// pattern covers is reached through the ancestor walk, one
		// directory at a time, in matchesHow.
		return matchSegmentsExact(p.segments, segs)
	}
	if len(segs) == 0 {
		return false
	}
	return matchSegmentsExact(p.segments, segs[len(segs)-1:])
}

// matchSegmentsExact matches pattern segments against path segments
// with the pattern consuming the whole path: it cannot end early and
// name everything below what it describes, which is the ancestor
// walk's job in matchesHow. A "**" segment still spans as many path
// segments as it needs, because that is the pattern naming those
// paths, not a containment rule reaching below it.
func matchSegmentsExact(pat, segs []string) bool {
	if !containsStarStar(pat) {
		return len(pat) == len(segs) && matchSegmentsPlain(pat, segs)
	}
	memo := make(map[[2]int]bool)
	return matchSegmentsExactAt(pat, 0, segs, 0, memo)
}

// matchSegmentsExactAt is the memoized recursion behind
// matchSegmentsExact: "**" spans zero or more segments, and the match
// is only exact when the path is exhausted too.
func matchSegmentsExactAt(pat []string, pi int, segs []string, si int, memo map[[2]int]bool) bool {
	key := [2]int{pi, si}
	if v, ok := memo[key]; ok {
		return v
	}
	res := false
	defer func() { memo[key] = res }()
	if pi == len(pat) {
		// Exact: the pattern consumed the path, and nothing in the
		// path is left unconsumed.
		res = si == len(segs)
		return res
	}
	if pat[pi] == "**" {
		if pi == len(pat)-1 {
			// A trailing "**" swallows every remaining segment, so
			// it is exact whenever at least one is left for it, the
			// same "at least one" rule as the containment walk.
			res = si < len(segs)
			return res
		}
		if matchSegmentsExactAt(pat, pi+1, segs, si, memo) {
			res = true
			return true
		}
		for j := si + 1; j <= len(segs); j++ {
			if matchSegmentsExactAt(pat, pi+1, segs, j, memo) {
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
	res = matchSegmentsExactAt(pat, pi+1, segs, si+1, memo)
	return res
}

// matchSegmentsPlain is matchSegmentsExact for patterns without
// "**": a plain recursion, one step per segment, no allocation.
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
			// git's fnmatch negates a class with "!" or "^"; a
			// literal "^" member is written "[]^...]" or escaped.
			if strings.HasPrefix(body, "!") || strings.HasPrefix(body, "^") {
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
// learn why. Every unescaped "[" that opens a class is checked, so
// the promise holds for a class left open behind a valid one
// ("[ab]x[y") and not just the first bracket of the line; a bracket
// inside a class that closes is a member and is never re-checked.
func parseGitignoreSegments(body string, lineNo int, raw string) ([]string, error) {
	for i := 0; i < len(body); {
		if body[i] != '[' || (i > 0 && body[i-1] == BS) {
			// An escaped "\[" names a literal bracket, not a
			// class, so only an unescaped "[" opens one.
			i++
			continue
		}
		// A class that closes ends at its "]": skip past it, the
		// way the matcher consumes it, instead of re-scanning its
		// members. A "[" inside a closed class is a member naming
		// a literal bracket, the way git's fnmatch reads "[[]",
		// not the start of another class.
		if _, rest, ok := splitClass(body[i:]); !ok {
			return nil, fmt.Errorf("exclude pattern %d: %q has an unterminated character class", lineNo, raw)
		} else {
			i = len(body) - len(rest)
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
