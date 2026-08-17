//go:build unix

package main

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestParseArgsShellForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want shellArgs
	}{
		{
			name: "bare -c",
			args: []string{"-c", "echo hi"},
			want: shellArgs{haveCommand: true, command: "echo hi", operands: []string{}},
		},
		{
			// How every tool spawns a login shell for one command.
			name: "clustered -lc",
			args: []string{"-lc", "echo hi"},
			want: shellArgs{haveCommand: true, login: true, command: "echo hi", operands: []string{}},
		},
		{
			// What Claude Code spawns, and what the flag package got wrong:
			// the command string is an operand, so options may precede it.
			name: "option after -c",
			args: []string{"-c", "-l", "echo hi"},
			want: shellArgs{haveCommand: true, login: true, command: "echo hi", operands: []string{}},
		},
		{
			name: "-ilc clusters three",
			args: []string{"-ilc", "echo hi"},
			want: shellArgs{haveCommand: true, login: true, interactive: true, command: "echo hi", operands: []string{}},
		},
		{
			// POSIX: the first operand after the command string is $0.
			name: "command with positional parameters",
			args: []string{"-c", "echo $1", "_", "one"},
			want: shellArgs{haveCommand: true, command: "echo $1", operands: []string{"_", "one"}},
		},
		{
			name: "script file and its arguments",
			args: []string{"script.sh", "-v", "arg"},
			want: shellArgs{operands: []string{"script.sh", "-v", "arg"}},
		},
		{
			name: "-- ends the options",
			args: []string{"-l", "--", "-not-an-option"},
			want: shellArgs{login: true, operands: []string{"-not-an-option"}},
		},
		{
			name: "a bare dash is an operand",
			args: []string{"-"},
			want: shellArgs{operands: []string{"-"}},
		},
		{
			name: "long options in both spellings",
			args: []string{"--sandbox", "workspace", "-rc", "/tmp/rc"},
			want: shellArgs{sandbox: "workspace", rc: "/tmp/rc", operands: []string{}},
		},
		{
			name: "long option with =value",
			args: []string{"--sandbox=none", "--version"},
			want: shellArgs{sandbox: "none", version: true, operands: []string{}},
		},
		{
			// -restore is a long name, not a cluster of -r -e -s -t…
			name: "long name that looks like a cluster",
			args: []string{"-restore", "abc123"},
			want: shellArgs{restore: "abc123", operands: []string{}},
		},
		{
			name: "no arguments at all",
			args: nil,
			want: shellArgs{operands: []string{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseArgs(tc.args)
			if err != nil {
				t.Fatalf("parseArgs(%q): %v", tc.args, err)
			}
			if !slices.Equal(got.operands, tc.want.operands) {
				t.Errorf("operands = %q, want %q", got.operands, tc.want.operands)
			}
			// Compared above; nil and empty mean the same thing here.
			got.operands, tc.want.operands = nil, nil
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseArgs(%q) =\n  %+v\nwant\n  %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseArgsRejections(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-c"},             // -c with nothing to run
		{"-x", "echo hi"},  // unknown short
		{"-lx", "echo hi"}, // unknown short inside a cluster
		{"--nope"},         // unknown long
		{"--sandbox"},      // long option missing its value
		{"--version=yes"},  // long option given a value it has none for
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) should have failed", args)
		}
	}
}

func TestParseArgsHelp(t *testing.T) {
	t.Parallel()
	if _, err := parseArgs([]string{"--help"}); !errors.Is(err, errHelp) {
		t.Errorf("--help = %v, want errHelp", err)
	}
}
