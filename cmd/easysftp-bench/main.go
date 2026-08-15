// Command easysftp-bench measures a benchmark run and turns it into the
// documents it stores.
//
// It is the whole benchmark harness issue #190 moved off the shell: "standard"
// and "matrix" generate the payloads, run the builds, shape and probe the link
// and aggregate what came out, "store" files a result set under benchmarks/,
// and "aggregate" is the seam between the two, callable on its own so a
// measurement can be re-aggregated from the manifest and JSONL it left behind.
//
// The subcommands that measure read their configuration from the environment,
// exactly as the shell scripts they replaced did; "aggregate" and "validate"
// read none, so given the same inputs they write the same bytes. That is what
// made the parity check of step 5 possible, and it is why a stored result can
// still be reproduced from its inputs.
//
// Usage:
//
//	easysftp-bench standard                              measure at the default settings
//	easysftp-bench matrix                                sweep connections against concurrency
//	easysftp-bench store                                 file a result set under benchmarks/
//	easysftp-bench aggregate --manifest <path> --out <dir>
//	easysftp-bench validate <stored-result.json>...
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "standard":
		err = runStandard()
	case "matrix":
		err = runMatrix()
	case "store":
		err = runStore()
	case "aggregate":
		err = aggregate(os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		// The GitHub Actions annotation format, like every other error the
		// benchmark scripts produce.
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `easysftp-bench measures a benchmark run and stores what it measured.

  standard                                  measure one or two builds at the default settings
  matrix                                    sweep connections against concurrency
  store                                     file a measured result set under benchmarks/
  aggregate --manifest <path> --out <dir>   write the result, the CSV and the summary
  validate <file>...                        check stored results against the schema

The first three read their configuration from the environment; see
benchmarks/README.md and .github/workflows/benchmark.yml.
`)
}

func aggregate(args []string) error {
	flags := flag.NewFlagSet("aggregate", flag.ExitOnError)
	manifestPath := flags.String("manifest", "", "the run manifest the measuring script wrote")
	outDir := flags.String("out", "", "directory the result, CSV and summary are written to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" || *outDir == "" {
		return fmt.Errorf("aggregate needs both --manifest and --out")
	}

	manifest, err := benchmark.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	inputs, err := benchmark.Load(manifest)
	if err != nil {
		return err
	}

	_, err = benchmark.Write(manifest, inputs, *outDir)
	return err
}

// validate checks stored results against the schema. It is what keeps "current
// stored results remain readable" from being a claim nobody runs.
func validate(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("validate needs at least one file")
	}
	failures := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			failures++
			continue
		}
		env, err := schema.DecodeStrict[schema.Envelope](data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			failures++
			continue
		}
		if err := env.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			failures++
			continue
		}
		fmt.Printf("%s: ok\n", path)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d stored result(s) did not validate", failures, len(paths))
	}
	return nil
}
