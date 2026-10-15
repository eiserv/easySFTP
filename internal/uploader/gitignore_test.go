package uploader

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustCompileGitignore(t *testing.T, lines ...string) *gitignoreMatcher {
	t.Helper()
	m, err := compileGitignore(lines)
	if err != nil {
		t.Fatalf("compileGitignore(%q): %v", lines, err)
	}
	return m
}

// bs is one backslash, a constant so the case table below reads as
// patterns instead of a run of escapes.
const bs = string(rune(BS))

// gitignoreCases are the (patterns, path, wantExcluded) rows issue #314
// differential-tested against git check-ignore and found wrong under the
// old matcher, plus the neighborhoods of each rule they exercise. isDir
// marks a directory path, which trailing-slash patterns need.
var gitignoreCases = []struct {
	patterns []string
	path     string
	isDir    bool
	want     bool
}{
	// "?" is a one-character wildcard, not a literal question mark.
	{[]string{"file?.txt"}, "file1.txt", false, true},
	{[]string{"file?.txt"}, "file12.txt", false, false},
	{[]string{"*.php?"}, "x.php5", false, true},
	{[]string{"*.php?"}, "x.php", false, false},
	// Parentheses, plus signs, dollars and carets are name characters,
	// not regex metacharacters.
	{[]string{"* (1).*"}, "photo (1).jpg", false, true},
	{[]string{"* (1).*"}, "photo 1.jpg", false, false},
	{[]string{"notes+draft.md"}, "notes+draft.md", false, true},
	{[]string{"c++/"}, "c++/main.cpp", false, true},
	{[]string{"c++/"}, "c++", true, true},
	{[]string{"c++/"}, "c++", false, false},
	{[]string{"$HOME"}, "$HOME", false, true},
	{[]string{"^draft"}, "^draft", false, true},
	// A slash in the middle anchors the pattern at the root.
	{[]string{"docs/internal"}, "docs/internal/a.md", false, true},
	{[]string{"docs/internal"}, "src/docs/internal/a.md", false, false},
	{[]string{"config/prod.json"}, "vendor/x/config/prod.json", false, false},
	{[]string{"config/prod.json"}, "config/prod.json", false, true},
	{[]string{"foo/**"}, "x/foo/a", false, false},
	{[]string{"foo/**"}, "foo/a/b", false, true},
	// The allow-list idiom the issue quotes.
	{[]string{"*", "!*/", "!*.html"}, "a/b.css", false, true},
	{[]string{"*", "!*/", "!*.html"}, "a/b.html", false, false},
	{[]string{"*", "!*/", "!*.html"}, "a.html", false, false},
	// A leading space is part of the pattern; git does not trim it.
	{[]string{" leading"}, " leading", false, true},
	// Classes, including the negated form the old matcher inverted.
	{[]string{"*.[!o]"}, "a.c", false, true},
	{[]string{"*.[!o]"}, "a.o", false, false},
	// An escaped metacharacter is literal.
	{[]string{bs + "file?.txt"}, "file?.txt", false, true},
	{[]string{bs + "file?.txt"}, "file1.txt", false, true},
	// Anchoring by leading slash.
	{[]string{"/root.txt"}, "root.txt", false, true},
	{[]string{"/root.txt"}, "a/root.txt", false, false},
	// Directory-only patterns need the isDir flag.
	{[]string{"dist/"}, "dist", true, true},
	{[]string{"dist/"}, "dist/a.js", false, true},
	{[]string{"dist/"}, "dist", false, false},
	// A plain name matches at any depth, as git does.
	{[]string{"debug.log"}, "logs/debug.log", false, true},
	// Last match wins, and a negation re-includes.
	{[]string{"*.log", "!important.log"}, "important.log", false, false},
	{[]string{"*.log", "!important.log"}, "debug.log", false, true},
	// The deliberate divergence, pinned here and documented in
	// docs/configuration.md: re-include below an excluded directory.
	{[]string{"node_modules/", "!node_modules/keep.js"}, "node_modules/keep.js", false, false},
	{[]string{"node_modules/", "!node_modules/keep.js"}, "node_modules/other.js", false, true},
	// "**" in the middle bridges directories.
	{[]string{"docs/**/notes"}, "docs/a/b/notes", false, true},
	{[]string{"docs/**/notes"}, "docs/notes", false, true},

	// Rows pinned by the git probes run during this change; each guards a
	// rule the old matcher mistranslated or a fix in this change.
	{[]string{"foo" + bs + " "}, "foo ", false, true},
	{[]string{"foo" + bs + " "}, "foo", false, false},
	{[]string{"foo" + string(rune(9))}, "foo" + string(rune(9)), false, true},
	{[]string{"dist"}, "dist/a.js", false, true},
	{[]string{"dist"}, "x/dist/a.js", false, true},
	{[]string{"dist"}, "dist", false, true},
	{[]string{"dist"}, "distx/a.js", false, false},
	{[]string{"*", "!dist"}, "dist/a.js", false, true},
	{[]string{"*", "!dist"}, "dist", false, false},
	{[]string{"c++/"}, "x/c++/main.cpp", false, true},
	{[]string{"foo/**"}, "foo", false, false},
	{[]string{"foo/**"}, "foo/a", false, true},
	{[]string{"docs/**/notes"}, "docs/a/b/notes", false, true},
	{[]string{"node_modules/"}, "x/node_modules/keep.js", false, true},

	// An earlier re-include does not beat a later directory exclude:
	// the directory exclude is the later, more specific line, so the
	// path is excluded, exactly as git decides it. Found by extending
	// the differential in review; pinned here so the regression the
	// review caught (re-include winning by position alone) cannot
	// return.
	{[]string{"*.txt", "!important.txt", "secrets/"}, "secrets/important.txt", false, true},
	{[]string{"*.txt", "!important.txt", "secrets/"}, "secrets/other.txt", false, true},
	{[]string{"*.txt", "!important.txt", "secrets/"}, "important.txt", false, false},
	// The allow-list idiom from the issue, followed by a deny line:
	// the deny line is last, so it wins below the directory too.
	{[]string{"*", "!*/", "!*.html", "private/"}, "private/a.html", false, true},
	{[]string{"*", "!*/", "!*.html", "private/"}, "x/private/a.html", false, true},
	{[]string{"*", "!*/", "!*.html", "private/"}, "a.html", false, false},
	// The documented divergence still holds when the re-include is
	// the later line: the leaf re-include wins below the excluded
	// directory only in that order.
	{[]string{"secrets/", "!secrets/important.txt"}, "secrets/important.txt", false, false},
	// An anchored re-include matches exactly: it re-includes the
	// paths it names, not everything below a directory it names.
	// git excludes dist/a.js here via "*", and so does the matcher.
	{[]string{"*", "!/dist"}, "dist", true, false},
	{[]string{"*", "!/dist"}, "dist/a.js", false, true},
	// An unanchored negated name matches its basename at any depth,
	// the way an unanchored plain one does; only the name it spells
	// is re-included, not a subtree.
	{[]string{"*", "!dist"}, "dist", true, false},
	{[]string{"*", "!keep.js"}, "logs/keep.js", false, false},
	{[]string{"*", "!keep.js"}, "logs/debug.log", false, true},
	// "[^o]" negates the class the way "[!o]" does; a caret at the
	// front of a class is not a literal member.
	{[]string{"[^o]x"}, "ax", false, true},
	{[]string{"[^o]x"}, "ox", false, false},
	{[]string{"*.[^o]"}, "a.c", false, true},
	{[]string{"*.[^o]"}, "a.o", false, false},
}

func TestGitignoreMatcherFollowsTheRules(t *testing.T) {
	for _, tc := range gitignoreCases {
		m := mustCompileGitignore(t, tc.patterns...)
		got, _ := m.matchesHow(tc.path, tc.isDir)
		if got != tc.want {
			t.Errorf("patterns %q, path %q (dir=%v): excluded=%v, want %v",
				tc.patterns, tc.path, tc.isDir, got, tc.want)
		}
	}
}

// TestGitignoreInvalidPatternsFailTheRun covers the fail-closed half of
// issue #314: a pattern the matcher cannot interpret must stop the run
// with the offending line named, never disappear quietly.
func TestGitignoreInvalidPatternsFailTheRun(t *testing.T) {
	for _, bad := range []string{
		"flags[",        // unterminated class
		"flags[!",       // negated, still unterminated
		"trailing" + bs, // lone trailing backslash
	} {
		if _, err := compileGitignore([]string{bad}); err == nil {
			t.Errorf("pattern %q: expected an error, got none", bad)
		}
	}
	// A name that merely looks like broken regex is not an error.
	if _, err := compileGitignore([]string{"secret(old"}); err != nil {
		t.Errorf("secret(old should compile: %v", err)
	}
	if _, err := compileGitignore([]string{"a" + bs + "|b"}); err != nil {
		t.Errorf("escaped pipe should compile: %v", err)
	}
}

// TestGitignoreDifferentialAgainstGit re-runs the comparison that wrote
// issue #314, as a test: one tree per case, the reference answer from
// "git check-ignore --no-index", and the matcher under the planner call
// pattern. Rows on the documented divergence (a re-include below an
// excluded directory) are skipped, because git cannot re-include there at
// all. Skipped when git is not on PATH, which keeps local runs hermetic;
// CI has git.
func TestGitignoreDifferentialAgainstGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; the in-memory table covers the rules")
	}
	for _, tc := range gitignoreCases {
		if onDivergence(tc.patterns, tc.path, tc.isDir) {
			continue
		}
		dir := t.TempDir()
		// check-ignore evaluates patterns against the working
		// tree, so it needs a repository even with --no-index; a bare
		// init is enough, nothing is committed or staged.
		if out, err := exec.Command(git, "init", "-q", dir).CombinedOutput(); err != nil {
			t.Skipf("git init failed in the temp dir: %v (%s)", err, out)
		}
		// Nothing is placed on disk except a directory an isDir row names:
		// check-ignore answers a path that is not on disk by the pattern
		// alone (verified against git 2.x), and only a trailing-slash
		// pattern matching the path itself needs the directory to exist.
		// Skipping file creation also keeps rows whose names Windows
		// cannot hold ("file?.txt") runnable there.
		full := filepath.Join(dir, tc.path)
		if tc.isDir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		body := ""
		for _, p := range tc.patterns {
			body += p + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(git, "check-ignore", "--no-index", "--", tc.path)
		cmd.Dir = dir
		err = cmd.Run()
		gitSays := false
		if err == nil {
			gitSays = true
		} else if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			gitSays = false
		} else {
			t.Fatalf("git check-ignore %q: %v", tc.path, err)
		}
		m := mustCompileGitignore(t, tc.patterns...)
		got, _ := m.matchesHow(tc.path, tc.isDir)
		if got != gitSays {
			t.Errorf("patterns %q, path %q (dir=%v): matcher says %v, git says %v",
				tc.patterns, tc.path, tc.isDir, got, gitSays)
		}
	}
}

// hasReinclude reports whether any pattern is a "!..." line.
func hasReinclude(patterns []string) bool {
	for _, p := range patterns {
		if len(p) > 0 && p[0] == '!' {
			return true
		}
	}
	return false
}

// onDivergence reports whether a row sits exactly on the documented
// divergence: the matcher re-includes the path (a later "!" line won),
// while git can never reach the path at all, because a plain pattern
// excluded one of its ancestor directories and git prunes there. Only
// those rows are skipped in the differential; a row below an excluded
// directory that the matcher still excludes (the re-include came
// earlier, so the exclude is the later line) agrees with git and must
// run. The old shape of this check (any dir/ pattern plus any "!" line)
// skipped far more than the divergence and let the review regression
// through: the differential never ran the rows that would have caught
// it.
func onDivergence(patterns []string, path string, isDir bool) bool {
	if !hasReinclude(patterns) {
		return false
	}
	m, err := compileGitignore(patterns)
	if err != nil {
		// The plain table test reports compile errors; here, failing
		// open to running the row keeps the differential honest.
		return false
	}
	if got, _ := m.matchesHow(path, isDir); got {
		// The matcher excludes it; git prunes or matches the same
		// way, so the row is comparable and must run.
		return false
	}
	segs := strings.Split(path, "/")
	for k := 1; k < len(segs); k++ {
		if excluded, pat := m.matchesHow(strings.Join(segs[:k], "/"), true); excluded &&
			pat != nil && !pat.negate {
			return true
		}
	}
	return false
}

// mustCompileGitignoreB is mustCompileGitignore for benchmarks.
func mustCompileGitignoreB(b *testing.B, lines ...string) *gitignoreMatcher {
	b.Helper()
	m, err := compileGitignore(lines)
	if err != nil {
		b.Fatalf("compileGitignore(%q): %v", lines, err)
	}
	return m
}
