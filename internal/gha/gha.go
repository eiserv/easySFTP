// Package gha provides helpers for GitHub Actions workflow commands,
// step outputs and job summaries.
package gha

import (
	"fmt"
	"os"
	"strings"
)

// escapeData escapes a value for use in a workflow command payload.
func escapeData(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	return r.Replace(s)
}

// Infof prints a plain log line.
func Infof(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

// Noticef prints a notice annotation.
func Noticef(format string, args ...any) {
	fmt.Printf("::notice::%s\n", escapeData(fmt.Sprintf(format, args...)))
}

// Warningf prints a warning annotation.
func Warningf(format string, args ...any) {
	fmt.Printf("::warning::%s\n", escapeData(fmt.Sprintf(format, args...)))
}

// Errorf prints an error annotation.
func Errorf(format string, args ...any) {
	fmt.Printf("::error::%s\n", escapeData(fmt.Sprintf(format, args...)))
}

// Group starts a collapsible log group.
func Group(name string) {
	fmt.Printf("::group::%s\n", escapeData(name))
}

// EndGroup closes the current log group.
func EndGroup() {
	fmt.Println("::endgroup::")
}

// SetOutput writes a step output. It is a no-op outside of GitHub Actions.
func SetOutput(name, value string) {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		Warningf("could not write output %s: %v", name, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s=%s\n", name, escapeData(value))
}

// AppendSummary appends markdown to the job summary. It is a no-op outside
// of GitHub Actions.
func AppendSummary(markdown string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		Warningf("could not write job summary: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, markdown)
}
