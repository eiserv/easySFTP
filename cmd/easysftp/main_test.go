package main

import "testing"

func TestHelpRequested(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "long", args: []string{"--help"}, want: true},
		{name: "short", args: []string{"-h"}, want: true},
		{name: "none", args: nil, want: false},
		{name: "extra", args: []string{"--help", "unexpected"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := helpRequested(tt.args); got != tt.want {
				t.Fatalf("helpRequested(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
