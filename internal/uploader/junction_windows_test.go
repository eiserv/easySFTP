//go:build windows

package uploader

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// trimFinalPath keeps a local volume path's drive letter and turns a UNC
// final path back into the \\server\share spelling; stripping only the
// generic prefix used to corrupt network targets into relative paths.
func TestTrimFinalPath(t *testing.T) {
	cases := map[string]string{
		`\\?\C:\dir\junction`: `C:\dir\junction`,
		`\\?\UNC\srv\share\d`: `\\srv\share\d`,
		`C:\plain\path`:       `C:\plain\path`,
	}
	for in, want := range cases {
		if got := trimFinalPath(in); got != want {
			t.Errorf("trimFinalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A symlink whose target is a junction: EvalSymlinks resolves the symlink
// but stops at the junction, so the walked root lands on a reparse point
// and, without the resolution loop, the plan came back empty - clean and
// sync then reconciled an empty tree against the remote target.
func TestSymlinkToJunctionChainIsWalkedThrough(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows concept")
	}

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "x", "app.js": "y"})

	parent := t.TempDir()
	junction := filepath.Join(parent, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, local).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction on this machine: %v (%s)", err, out)
	}
	symlink := filepath.Join(parent, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/D", symlink, junction).CombinedOutput(); err != nil {
		t.Skipf("cannot create a directory symlink on this machine (needs the SeCreateSymbolicLink privilege): %v (%s)", err, out)
	}

	matcher := mustCompileGitignore(t)
	p, err := buildPlan(config.UploadPair{Local: symlink, Remote: "/www"}, config.StrategyOverlay, planOptions{matcher: matcher, pruneDirs: true, manifestName: manifestName})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.files) != 2 {
		t.Fatalf("expected the 2 files behind the symlink->junction chain, got %d (%+v)", len(p.files), p.files)
	}
	if p.skippedNonRegular != 0 {
		t.Errorf("the chain root was counted as skipped non-regular: %d", p.skippedNonRegular)
	}
}
