package credentials

import (
	"slices"
	"strings"
	"testing"
)

// screenOf feeds each of writes to a w x h screen in turn and gives the
// lines it reported, the last report whole.
func screenOf(w, h int, writes ...string) (*screen, []string) {
	var got []string
	s := newScreen(w, h, func(l string) { got = append(got, l) })
	for _, p := range writes {
		s.Write([]byte(p))
		s.report(true)
	}
	s.report(false)
	return s, got
}

func TestScreen(t *testing.T) {
	for name, c := range map[string]struct {
		writes []string
		want   []string
	}{
		// Words placed by column, as claude 2.1.321 draws them (2026-11-15).
		"columns": {[]string{"Your\x1b[7GOAuth\x1b[13Gtoken\r\n"}, []string{"Your  OAuth token"}},
		// A cell that shows the right character already is skipped: the
		// token is read from the screen, not from the bytes.
		"skipped cells": {[]string{"\x1b[2;8Hoat01\r\n", "\x1b[2;1Hsk-ant-\x1b[5C-AAAABBBB\x1b[K\r\r\n"},
			[]string{"       oat01", "sk-ant-oat01-AAAABBBB"}},
		"overwrite": {[]string{"hello world\rjelly\r\n"}, []string{"hello world", "jelly world"}},
		"erase line": {[]string{"hello world\r\x1b[5C\x1b[K\r\n", "abc\x1b[1K\r\n", "xyz\x1b[2K\r\n"},
			[]string{"hello world", "hello", "", ""}},
		// A line erased before it was ended is not read.
		"erase screen": {[]string{"one\r\ntwo\x1b[2J\x1b[Hthree\r\n"}, []string{"one", "three", ""}},
		"cursor moves": {[]string{"abc\x1b[2D\x1b[1B\x1b[3Cz\x1b[2Ay\r\n"}, []string{"abc  y", "    z"}},
		"save and restore": {[]string{"ab\x1b7\x1b[5;9Hx\x1b8c\x1b[s\x1b[3;3Hq\x1b[ud\r\n"},
			[]string{"abcd", "  q     x"}},
		"backspace": {[]string{"abc\b\bX\r\n"}, []string{"aXc"}},
		// OSC 8: the link's visible text is what is read.
		"hyperlink": {[]string{"\x1b]8;;https://example.test/a\x1b\\https://example.test/a\x1b]8;;\x1b\\ and \x1b]8;;https://example.test/b\x07b\x1b]8;;\x07\r\n"},
			[]string{"https://example.test/a and b"}},
		"styles and charsets":     {[]string{"\x1b[1;32mgre\x1b(Ben\x1b[0m\x1bP1;2|data\x1b\\!\r\n"}, []string{"green!"}},
		"utf-8 cut across writes": {[]string{"caf\xc3", "\xa9 \xe2\x9c", "\x93\r\n"}, []string{"café ✓"}},
		"wide":                    {[]string{"日本x\r\n"}, []string{"日本x"}},
		// A line longer than the screen wraps and is read whole.
		"wrapped": {[]string{strings.Repeat("a", 70) + "\r\n"}, []string{strings.Repeat("a", 70)}},
		// Rows that scrolled off the top are read once, before they go.
		"scroll": {[]string{"1\r\n2\r\n3\r\n4\r\n5\r\n"}, []string{"1", "2", "3", "4", "5"}},
		// A half-drawn line waits for its end: a token cut across reads.
		"half drawn": {[]string{"Token: sk-fa", "ke-oat01\r\n"}, []string{"Token: sk-fake-oat01"}},
		// An unchanged line is not read again.
		"redrawn": {[]string{"same\r", "same\r", "same\r\n"}, []string{"same"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, got := screenOf(30, 3, c.writes...); !slices.Equal(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Bracketed paste is on from CSI ? 2004 h until CSI ? 2004 l, also when it
// is set with other modes.
func TestScreenPasteMode(t *testing.T) {
	s, _ := screenOf(10, 3)
	for _, c := range []struct {
		write string
		want  bool
	}{
		{"\x1b[?25l", false}, {"\x1b[?2004h", true}, {"\x1b[?25h", true},
		{"\x1b[?1;2004l", false}, {"\x1b[?1004;2004h", true}, {"\x1b[2004l", true},
	} {
		s.Write([]byte(c.write))
		if s.paste != c.want {
			t.Fatalf("after %q: paste %v", c.write, s.paste)
		}
	}
}
