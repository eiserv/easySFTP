package scenario_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/scenario"
)

func TestCalibrationGrammar(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		kib   int
	}{
		{"calib-10x64k", 10, 64},
		{"calib-1000x16m", 1000, 16384},
	} {
		groups, err := scenario.Spec(tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(groups) != 1 || groups[0].Count != tc.count || groups[0].KiB != tc.kib {
			t.Errorf("%s parsed as %+v, want %d x %d KiB", tc.name, groups, tc.count, tc.kib)
		}
	}
	for _, name := range []string{"calib-nonsense", "calib-10x64", "calib-0x64k", "calib-10x0k", "calib-x64k"} {
		if _, err := scenario.Spec(name); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The per-scenario grid of issue #184 phase 5. Both rules are properties of the
// payload: an axis value above the file count is the same configuration twice,
// and request_concurrency is per file, so small files cannot use it at all.
func TestPerScenarioAxes(t *testing.T) {
	if files, _ := scenario.Files("mixed"); files != 56 {
		t.Errorf("the file count is summed over the payload groups: got %d, want 56", files)
	}
	if max, _ := scenario.MaxKiB("mixed"); max != 2048 {
		t.Errorf("the largest file of a payload decides the request axis: got %d, want 2048", max)
	}
	if sweeps, _ := scenario.SweepsRequests("single"); !sweeps {
		t.Error("the request axis must apply where a file is large enough")
	}
	if sweeps, _ := scenario.SweepsRequests("small"); sweeps {
		t.Error("the request axis must not apply to a payload of 4 KiB files")
	}

	for _, tc := range []struct {
		name string
		want []int
	}{
		{"single", []int{1}}, // capped and deduplicated at the file count
		{"small", []int{1, 2, 4, 8}},
		{"large", []int{1, 2}}, // a partial cap keeps the values below it
	} {
		got, err := scenario.AxisFor(tc.name, []int{1, 2, 4, 8})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !equal(got, tc.want) {
			t.Errorf("%s: axis %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want scenario.Shape
	}{
		{"redeploy", scenario.Shape{Mode: "overlay", Prepopulate: true, Layout: scenario.LayoutFlat}},
		{"sync", scenario.Shape{Mode: "sync", Prepopulate: true, Layout: scenario.LayoutFlat}},
		{"deep", scenario.Shape{Mode: "overlay", Prepopulate: false, Layout: scenario.LayoutDeep}},
		// The scenarios that predate this table keep the old shape.
		{"small", scenario.Shape{Mode: "overlay", Prepopulate: false, Layout: scenario.LayoutFlat}},
	} {
		if got := scenario.ShapeOf(tc.name); got != tc.want {
			t.Errorf("%s: shape %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestGenerateAndMutate(t *testing.T) {
	dir := t.TempDir()
	quiet := func(string, ...any) {}
	if err := scenario.Generate(dir, []string{"deep", "calib-10x64k"}, quiet, quiet); err != nil {
		t.Fatalf("generating: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "empty")); err != nil {
		t.Errorf("the empty payload the pre-clean deploys is missing: %v", err)
	}

	files := list(t, filepath.Join(dir, "deep"))
	if len(files) != 400 {
		t.Errorf("the deep payload has %d files, want the 400 of its spec", len(files))
	}
	// 7 levels of two directories each: many directories holding a handful of
	// files, which is what separates create_dirs cost from transfer cost.
	if got := countDirs(t, filepath.Join(dir, "deep"), 7); got != 128 {
		t.Errorf("the deep payload has %d leaf directories, want 128", got)
	}
	if got := countDirs(t, filepath.Join(dir, "deep"), 8); got != 0 {
		t.Errorf("%d directories sit deeper than 7 levels", got)
	}

	calib := filepath.Join(dir, "calib-10x64k")
	for _, path := range list(t, calib) {
		if size(t, path) != 64*1024 {
			t.Errorf("%s is %d bytes; a calibration payload is uniform", path, size(t, path))
		}
	}

	scenario.Mutate(calib, scenario.ChangedFiles)
	changed := 0
	for _, path := range list(t, calib) {
		if size(t, path) == 64*1024+512 {
			changed++
		}
	}
	if changed != scenario.ChangedFiles {
		t.Errorf("the mutation changed %d files, want %d", changed, scenario.ChangedFiles)
	}
}

func list(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

func countDirs(t *testing.T, root string, depth int) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if depthOf(rel) == depth {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return count
}

func depthOf(rel string) int {
	depth := 1
	for _, r := range rel {
		if r == filepath.Separator || r == '/' {
			depth++
		}
	}
	return depth
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
