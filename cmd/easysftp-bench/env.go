package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The measuring commands are configured through the environment, the way the
// shell scripts they replace were, so .github/workflows/benchmark.yml keeps
// passing the same variables and a maintainer keeps invoking them the same way.
// Everything here is reading and validating; nothing below this file touches
// os.Getenv.

// requireEnv fails on the first empty variable, listing all of them, because a
// benchmark that dies on the second missing secret after minutes of measuring
// is a worse error message than one that dies on all of them at once.
func requireEnv(names ...string) error {
	var missing []string
	for _, name := range names {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	for _, name := range missing {
		fmt.Fprintf(os.Stderr, "::error::%s is required but empty\n", name)
	}
	return fmt.Errorf("see the secret list in .github/workflows/benchmark.yml")
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// envPositive reads a variable that must be a positive integer, or its default
// when unset.
func envPositive(name string, fallback int) (int, error) {
	text := os.Getenv(name)
	if text == "" {
		return fallback, nil
	}
	value, err := positiveInt(text)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive integer, got '%s'", name, text)
	}
	return value, nil
}

// envAxis reads a whitespace-separated list of positive integers.
func envAxis(name, fallback string) ([]int, string, error) {
	raw := envOr(name, fallback)
	var out []int
	for _, field := range strings.Fields(raw) {
		value, err := positiveInt(field)
		if err != nil {
			return nil, "", fmt.Errorf("matrix axis values must be positive integers, got '%s'", field)
		}
		out = append(out, value)
	}
	return out, raw, nil
}

// envRequestAxis reads the third matrix axis, which carries the literal token
// "default" for "set nothing and let easySFTP pick". That keeps a
// two-dimensional grid expressible now that the axis has a real default, and it
// is stored as a null coordinate.
func envRequestAxis(name, fallback string) ([]*int, string, error) {
	raw := os.Getenv(name)
	fields := strings.Fields(envOr(name, fallback))
	if len(fields) == 0 {
		fields = []string{"default"}
	}
	out := make([]*int, 0, len(fields))
	for _, field := range fields {
		if field == "default" {
			out = append(out, nil)
			continue
		}
		value, err := positiveInt(field)
		if err != nil {
			return nil, "", fmt.Errorf(
				"request_concurrency axis values must be positive integers or the token 'default', got '%s'", field)
		}
		out = append(out, &value)
	}
	return out, raw, nil
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
