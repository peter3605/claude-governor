package main

import (
	"reflect"
	"testing"
)

// The fields lsof printed for the sampler on 2026-11-21, trimmed: cwd and txt are not
// numbered descriptors, sockets are not regular files, and only fd 1-4 survive.
func TestParseLsofFiles(t *testing.T) {
	out := "p8430\nfcwd\ntDIR\ni57828123\nk26\nn/Users/peterho/Repos/claude-governor\n" +
		"ftxt\ntREG\ni65738309\nk1\nn/Users/peterho/.local/share/claude-governor/bin/governor\n" +
		"f1\ntREG\ni58299485\nk1\nn/Users/peterho/.claude/governor/sampler.log\n" +
		"f4\ntREG\ni74051894\nk0\nn/Users/peterho/.claude/governor/samples.jsonl.1\n" +
		"f5\ntunix\nn->0x847795b7e648b23c\n"
	want := []openFile{
		{fd: "1", name: "/Users/peterho/.claude/governor/sampler.log", ino: 58299485, nlink: 1},
		{fd: "4", name: "/Users/peterho/.claude/governor/samples.jsonl.1", ino: 74051894, nlink: 0},
	}
	if got := parseLsofFiles(out); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}
