// Package scenario is the payload half of the benchmark: what a scenario is
// made of, how it is deployed, and which axis values its payload can use.
//
// It is the Go form of the scenario_* functions scripts/benchmark-lib.sh grew
// (issue #190, step 3). The tables are copied over unchanged on purpose: a
// scenario whose payload or shape moves during the rewrite makes every stored
// result before it incomparable, which is the one thing this migration may not
// do.
package scenario

import (
	"fmt"
	"strconv"
	"strings"
)

// ChangedFiles is how many files Mutate changes between the unmeasured deploy
// and the measured one. Small on purpose: "500 files, 3 changed" is the CI
// case, and a larger number would measure the upload again instead of the scan.
const ChangedFiles = 3

// RequestAxisMinKiB is the file size from which advanced.request_concurrency
// can do anything at all.
//
// That setting is sftp.MaxConcurrentRequestsPerFile (see
// internal/uploader/connection.go): how many write requests of *one* file may
// be in flight. pkg/sftp writes in 32 KiB packets, so a 4 KiB file is a single
// packet and cannot use a second request no matter what the value is, and a
// file needs 32 packets (1 MiB) before a value above easySFTP's default of 16
// has anything left to pipeline. Below this threshold the axis measures the
// same number repeatedly, which is what keeps it from tripling a grid it cannot
// move.
const RequestAxisMinKiB = 1024

// Layouts a payload can be written in.
const (
	LayoutFlat = "flat"
	LayoutDeep = "deep"
)

// Group is one "<count>:<KiB each>" group of a payload.
type Group struct {
	Count int
	KiB   int
}

// Shape is the three things that make a scenario a *deploy* rather than a
// payload.
//
//	Mode         the easySFTP mode the measured run uses
//	Prepopulate  the measured run is preceded by an unmeasured full deploy of
//	             the same tree, of which Mutate then changes a few files. That
//	             is what makes remote_scan, hash, manifest_read / manifest_write
//	             and the skip path measurable at all; without it every one of
//	             them runs against an empty target
//	Layout       flat (8 sibling directories) or deep (7 levels, few files each)
type Shape struct {
	Mode        string
	Prepopulate bool
	Layout      string
}

// Spec is a scenario's payload, as groups of "<count> files of <KiB> each".
//
// Fixed on purpose: a benchmark whose payload changes between runs measures
// nothing.
//
// "single" exists for the matrix benchmark: one large file cannot be spread
// over connections at all, which makes it the control against which a
// connections/concurrency curve is read.
//
// The scenarios below "single" are issue #184, phase 3: every result before
// them was a full upload into an empty target in mode overlay, which is the
// rarest real deploy. They cover the redeploy, the sync mode, a deep tree, a
// file count high enough for the per-run fixed costs to fall away, and a
// calibration family that is uniform by construction. See Shape for how a
// scenario is run, which for these is as much of the measurement as the payload
// is.
func Spec(name string) ([]Group, error) {
	switch {
	case name == "small": // per-file round-trip overhead dominates
		return []Group{{300, 4}}, nil
	case name == "mixed": // roughly a built website
		return []Group{{40, 16}, {12, 256}, {4, 2048}}, nil
	case name == "large": // raw transfer throughput
		return []Group{{2, 16384}}, nil
	case name == "single": // one 32 MiB file, no parallelism to find
		return []Group{{1, 32768}}, nil
	case name == "redeploy": // the CI case: almost nothing changed
		return []Group{{500, 4}}, nil
	case name == "sync": // the same payload, through mode sync
		return []Group{{500, 4}}, nil
	case name == "deep": // node_modules-shaped, see Shape
		return []Group{{400, 4}}, nil
	case name == "bulk": // per-file cost past the fixed costs
		return []Group{{2000, 4}}, nil
	case strings.HasPrefix(name, "calib-"):
		return calibSpec(name)
	default:
		return nil, fmt.Errorf("unknown scenario %s", name)
	}
}

// calibSpec parses the calibration family, "calib-<count>x<size>" with the size
// as <n>k or <n>m, e.g. calib-100x64k. One size per scenario, so
// "t_file = r * RTT + size / B" can be fitted against it; "mixed" mixes three
// sizes and structurally cannot give that (issue #184, phase 3).
func calibSpec(name string) ([]Group, error) {
	malformed := fmt.Errorf(
		"calibration scenario '%s' must look like calib-<count>x<size>, size in k or m (for example calib-100x64k)", name)

	rest := strings.TrimPrefix(name, "calib-")
	countText, sizeText, found := strings.Cut(rest, "x")
	if !found {
		return nil, malformed
	}
	count, err := positiveInt(countText)
	if err != nil {
		return nil, malformed
	}
	if sizeText == "" {
		return nil, malformed
	}
	unit := sizeText[len(sizeText)-1]
	size, err := positiveInt(sizeText[:len(sizeText)-1])
	if err != nil {
		return nil, malformed
	}
	switch unit {
	case 'k':
		return []Group{{count, size}}, nil
	case 'm':
		return []Group{{count, size * 1024}}, nil
	default:
		return nil, malformed
	}
}

// positiveInt accepts what the shell's ^[1-9][0-9]*$ accepted, and nothing
// else: no sign, no leading zero, no whitespace.
func positiveInt(text string) (int, error) {
	if text == "" || text[0] == '0' {
		return 0, fmt.Errorf("not a positive integer: %q", text)
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, fmt.Errorf("not a positive integer: %q", text)
		}
	}
	return strconv.Atoi(text)
}

// Description is the one-line summary the summary tables print.
func Description(name string) string {
	switch {
	case name == "small":
		return "300 files x 4 KiB"
	case name == "mixed":
		return "40 x 16 KiB + 12 x 256 KiB + 4 x 2 MiB"
	case name == "large":
		return "2 files x 16 MiB"
	case name == "single":
		return "1 file x 32 MiB"
	case name == "redeploy":
		return "500 x 4 KiB, redeployed over itself with 3 files changed, overlay plus advanced.skip_unchanged"
	case name == "sync":
		return "500 x 4 KiB, redeployed over itself with 3 files changed, mode sync"
	case name == "deep":
		return "400 x 4 KiB in a tree 7 directories deep, 1 to 3 files per directory"
	case name == "bulk":
		return "2000 files x 4 KiB"
	case strings.HasPrefix(name, "calib-"):
		groups, err := Spec(name)
		if err != nil {
			return "unknown"
		}
		return fmt.Sprintf("%d files x %d KiB, uniform (calibration)", groups[0].Count, groups[0].KiB)
	default:
		return "unknown"
	}
}

// ShapeOf reports how a scenario is deployed. Everything not listed keeps the
// shape every stored result was measured with, so the existing scenarios are
// untouched by this table existing.
//
// A prepopulated overlay scenario is measured with advanced.skip_unchanged on:
// an overlay redeploy without it re-uploads everything, which is the fresh
// upload the other scenarios already measure.
func ShapeOf(name string) Shape {
	switch name {
	case "redeploy":
		return Shape{Mode: "overlay", Prepopulate: true, Layout: LayoutFlat}
	case "sync":
		return Shape{Mode: "sync", Prepopulate: true, Layout: LayoutFlat}
	case "deep":
		return Shape{Mode: "overlay", Prepopulate: false, Layout: LayoutDeep}
	default:
		return Shape{Mode: "overlay", Prepopulate: false, Layout: LayoutFlat}
	}
}

// Files is how many files the payload has, summed over its groups.
func Files(name string) (int, error) {
	groups, err := Spec(name)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, g := range groups {
		total += g.Count
	}
	return total, nil
}

// MaxKiB is the largest single file in the payload, in KiB.
func MaxKiB(name string) (int, error) {
	groups, err := Spec(name)
	if err != nil {
		return 0, err
	}
	max := 0
	for _, g := range groups {
		if g.KiB > max {
			max = g.KiB
		}
	}
	return max, nil
}

// SweepsRequests reports whether the request_concurrency axis applies to this
// scenario, or whether it is measured at easySFTP's own default only.
func SweepsRequests(name string) (bool, error) {
	max, err := MaxKiB(name)
	if err != nil {
		return false, err
	}
	return max >= RequestAxisMinKiB, nil
}

// AxisFor is the requested axis values that can actually differ for this
// payload, in the order they were given.
//
// Only the per-file upload path spreads over connections and workers, so at
// most <file count> files are ever in flight: an axis value above that is the
// same configuration measured a second time under a different name. The stored
// "single" grid is the demonstration, 30 cells of 0.38 MiB/s for one 32 MiB
// file (issue #184, phase 5). Values are therefore capped at the file count and
// deduplicated, which is what buys the room for a longer concurrency axis where
// it can move and for the request axis where it can.
func AxisFor(name string, values []int) ([]int, error) {
	files, err := Files(name)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(values))
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value > files {
			value = files
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}
