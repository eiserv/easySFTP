// Command linkprobe measures the network path to an SFTP server and prints one
// JSON document (see internal/linkprobe) to stdout.
//
// It exists for the benchmark scripts, not for users of the action: the shipped
// release binaries are built from ./cmd/easysftp only. It shares no code with
// easySFTP's uploader, because a control measurement taken through the code
// under test would not be a control.
//
// Everything comes from the environment and nothing from flags, so a password
// never appears in a process listing:
//
//	LINKPROBE_HOST         server to probe                        (required)
//	LINKPROBE_USERNAME     SFTP user name                         (required)
//	LINKPROBE_PASSWORD     password                               (required)
//	LINKPROBE_KNOWN_HOSTS  verbatim "ssh-keyscan <host>" output    (required)
//	LINKPROBE_PORT         default 22
//	LINKPROBE_REMOTE_PATH  directory the probe may write into      (required for
//	                       the throughput control; without it only RTT is
//	                       measured)
//	LINKPROBE_RTT_SAMPLES     counted round-trips, default 21
//	LINKPROBE_CONTROL_BYTES   total payload per throughput pass, default 8388608
//	LINKPROBE_CONTROL_STREAMS parallel connections in the second pass, default 4
//	LINKPROBE_TIMEOUT         Go duration for the whole probe, default 5m
//
// The contract with the calling script is deliberately narrow: exit 0 means
// stdout is a valid JSON document, whatever went wrong inside it (the
// document's own "errors" array says). A non-zero exit means the environment
// was unusable and nothing was measured, and then stdout is empty.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/eiserv/easySFTP/internal/linkprobe"
)

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::linkprobe: %v\n", err)
		os.Exit(1)
	}

	// The error is intentionally not fatal: a link that was unreachable at
	// 14:02 is a measurement, and the report says so. Only stderr gets the
	// detail, since the calling script's log masks secrets and the report is
	// stored unmasked.
	report, runErr := linkprobe.Run(context.Background(), cfg)
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "::warning::linkprobe could not measure the link: %v\n", runErr)
	}

	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::linkprobe: encoding the report: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

func configFromEnv() (linkprobe.Config, error) {
	cfg := linkprobe.Config{
		Host:       os.Getenv("LINKPROBE_HOST"),
		User:       os.Getenv("LINKPROBE_USERNAME"),
		Password:   os.Getenv("LINKPROBE_PASSWORD"),
		KnownHosts: os.Getenv("LINKPROBE_KNOWN_HOSTS"),
		RemotePath: os.Getenv("LINKPROBE_REMOTE_PATH"),
	}
	for name, value := range map[string]string{
		"LINKPROBE_HOST":        cfg.Host,
		"LINKPROBE_USERNAME":    cfg.User,
		"LINKPROBE_PASSWORD":    cfg.Password,
		"LINKPROBE_KNOWN_HOSTS": cfg.KnownHosts,
	} {
		if value == "" {
			return cfg, fmt.Errorf("%s is required but empty", name)
		}
	}

	var err error
	if cfg.Port, err = envInt("LINKPROBE_PORT", 22); err != nil {
		return cfg, err
	}
	if cfg.RTTSamples, err = envInt("LINKPROBE_RTT_SAMPLES", 0); err != nil {
		return cfg, err
	}
	if cfg.ControlStreams, err = envInt("LINKPROBE_CONTROL_STREAMS", 0); err != nil {
		return cfg, err
	}
	bytes, err := envInt("LINKPROBE_CONTROL_BYTES", 0)
	if err != nil {
		return cfg, err
	}
	cfg.ControlBytes = int64(bytes)

	if raw := os.Getenv("LINKPROBE_TIMEOUT"); raw != "" {
		if cfg.Timeout, err = time.ParseDuration(raw); err != nil {
			return cfg, fmt.Errorf("LINKPROBE_TIMEOUT must be a Go duration such as 5m, got %q", raw)
		}
	}
	return cfg, nil
}

// envInt reads a non-negative integer, returning fallback when the variable is
// unset or empty. Zero means "let internal/linkprobe apply its own default",
// which is why zero is accepted rather than rejected as too small.
func envInt(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, raw)
	}
	return value, nil
}
