package main

// Text continuations always carry a real prefix and marker; other hostile
// controls cannot drive the terminal (issues #492 and #568).

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// What a hostile push name or message ID looks like: a line break followed by
// something shaped like a log entry of the bridge.
const (
	forgedName = "x\n12:00:00.000 [Bridge ERROR] the store was wiped"
	forgedID   = "3EB0\r\n12:00:00.000 [Bridge INFO] paired a new device"
)

func TestOneLine(t *testing.T) {
	unchanged := []string{
		"",
		"Updating chat name from PushName for sender@lid: Old -> New",
		"Alice da Silva — Example City",
		"family 👨‍👩‍👧 group",       // zero-width joiners inside an emoji sequence
		"שלום עולם, مرحبا بالعالم", // right-to-left scripts, no bidi control
		"日本語のグループ",
		"a\ttabbed\tline",
		// The directional marks right-to-left keyboards put next to a word.
		"name" + string(rune(0x200F)),
		string(rune(0x200E)) + "name" + string(rune(0x061C)),
	}
	for _, s := range unchanged {
		if got := oneLine(s); got != s {
			t.Errorf("oneLine(%q) = %q, want it unchanged", s, got)
		}
	}
	escaped := map[string]string{
		`C:\store\messages.db and \n`: `C:\\store\\messages.db and \\n`,
		"a\nb":                        `a\nb`,
		"a\r\nb":                      `a\r\nb`,
		"a\x00b":                      `a\x00b`,
		"red \x1b[31malert\x1b[0m":    `red \x1b[31malert\x1b[0m`, // a terminal escape sequence
		"a\x7fb":                      `a\x7fb`,
		"a\u0085b":                    `a\u0085b`, // NEL, the C1 "next line"
		"a\u2028b\u2029c":             `a\u2028b\u2029c`,
		"user\u202egpj.exe":           `user\u202egpj.exe`, // right-to-left override
		"a\u2066b\u2069":              `a\u2066b\u2069`,
		"bad \xff byte":               `bad \xff byte`,
		"é\nü":                        `é\nü`, // the text around an escape is kept as it is
		forgedName:                    `x\n12:00:00.000 [Bridge ERROR] the store was wiped`,
	}
	// An embedded run, the way some clients wrap a phone number: U+202A ... U+202C.
	escaped[string(rune(0x202A))+"+00 0000"+string(rune(0x202C))] = "\\u202a+00 0000\\u202c"
	for in, want := range escaped {
		got := oneLine(in)
		if got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "\n\r\x1b\x00") {
			t.Errorf("oneLine(%q) still carries a control character: %q", in, got)
		}
	}
}

// The JSON format keeps the message as it was sent: it is one JSON string, so
// it never needed the escaping and must not get a second layer of it.
func TestJSONLoggerKeepsTheMessageAsItIs(t *testing.T) {
	var buf bytes.Buffer
	l := newJSONLogger("bridge", "INFO", &buf)
	l.Infof("Updating chat name from PushName for %s: %s", "sender@lid", forgedName)
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("one call must print one line, got %d: %q", n, buf.String())
	}
	var line map[string]string
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if want := "Updating chat name from PushName for sender@lid: " + forgedName; line["msg"] != want {
		t.Errorf("msg = %q, want %q", line["msg"], want)
	}
}

// json.Marshal leaves DEL, the C1 controls and the bidirectional controls as
// they are. The JSON logger writes them as JSON escapes: the same message once
// decoded, and nothing in the line a terminal would act on.
func TestJSONLoggerLineCarriesNoTerminalControl(t *testing.T) {
	hostile := "name" + string(rune(0x7F)) + string(rune(0x85)) + string(rune(0x9B)) + "31m" + string(rune(0x202E)) + "gpj.exe" + string(rune(0x2066))
	ordinary := "Caf" + string(rune(0xE9)) + " " + string(rune(0x200F))
	var buf bytes.Buffer
	l := newJSONLogger("bridge", "INFO", &buf)
	l.Infof("%s / %s", hostile, ordinary)
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("one call must print one line, got %d: %q", n, buf.String())
	}
	for _, r := range buf.String() {
		if r == 0x7F || (r >= 0x80 && r <= 0x9F) || reordersText(r) {
			t.Errorf("the line carries U+%04X as it is: %q", r, buf.String())
		}
	}
	if !strings.Contains(buf.String(), ordinary) {
		t.Errorf("ordinary text must stay readable in the line: %q", buf.String())
	}
	var line map[string]string
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if want := hostile + " / " + ordinary; line["msg"] != want {
		t.Errorf("msg = %q, want %q", line["msg"], want)
	}
}

func TestTextLoggerMarksEveryContinuationAndKeepsStackReadable(t *testing.T) {
	var out bytes.Buffer
	logger := newTextWriter("Bridge", "INFO", &out, false)
	logger.Errorf("recovered panic: %s\ngoroutine 1 [running]:\n\tfile.go:42\n%s", "fake failure", forgedName)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("stack lines=%d", len(lines))
	}
	for index, line := range lines {
		_, payload, ok := strings.Cut(line, " [Bridge ERROR] ")
		if !ok {
			t.Fatal("line lost its real prefix")
		}
		if index > 0 && !strings.HasPrefix(payload, textLogContinuation) {
			t.Fatal("forged unmarked continuation")
		}
		if strings.ContainsAny(line, "\r\x1b\x00") {
			t.Fatal("unsafe control in rendered line")
		}
	}
	if !strings.Contains(lines[2], "[continued] \tfile.go:42") || !strings.Contains(lines[4], "[continued] 12:00:00.000 [Bridge ERROR]") {
		t.Fatal("stack or hostile continuation not readable and marked")
	}
}

type textCountingStringer struct{ calls *int }

func (s textCountingStringer) String() string { *s.calls++; return "fake formatted value" }

func TestTextLoggerFormatsOnceAndPreservesSubloggerLevel(t *testing.T) {
	var out bytes.Buffer
	logger := newTextWriter("Client", "WARN", &out, false)
	calls := 0
	logger.Debugf("%s", textCountingStringer{&calls})
	logger.Sub("Socket").Infof("%s", textCountingStringer{&calls})
	if calls != 0 || out.Len() != 0 {
		t.Fatal("discarded payload was formatted")
	}
	logger.Sub("Socket").Warnf("%s", textCountingStringer{&calls})
	if calls != 1 || !strings.Contains(out.String(), "[Client/Socket WARN] fake formatted value") {
		t.Fatal("level, formatting or submodule changed")
	}
}

func TestTextLoggerCapsCompleteLinesAtUTF8Boundary(t *testing.T) {
	for _, input := range []string{strings.Repeat("é", textLogMaxLine), strings.Repeat("\\", textLogMaxLine), strings.Repeat(" ", textLogMaxLine)} {
		var out bytes.Buffer
		newTextWriter(strings.Repeat("界", 1000), "INFO", &out, false).Infof("%s\nsecond", input)
		for _, line := range strings.SplitAfter(out.String(), "\n") {
			if line == "" {
				continue
			}
			if len(line) > textLogMaxLine || !utf8.ValidString(line) {
				t.Fatal("line cap or UTF-8 boundary violated")
			}
		}
		if !strings.Contains(out.String(), textLogTruncated) || !strings.Contains(out.String(), "[continued] second") {
			t.Fatal("truncation or continuation marker missing")
		}
	}
}

func TestTextLoggerKeepsConcurrentCallsTogether(t *testing.T) {
	var out bytes.Buffer
	logger := newTextWriter("Bridge", "INFO", &out, false)
	var done sync.WaitGroup
	for index := range 32 {
		done.Add(1)
		go func() { defer done.Done(); logger.Sub("Worker").Infof("call %d\ntail %d", index, index) }()
	}
	done.Wait()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 64 {
		t.Fatalf("lines=%d", len(lines))
	}
	for index := 0; index < len(lines); index += 2 {
		_, head, _ := strings.Cut(lines[index], "call ")
		if !strings.HasSuffix(lines[index+1], "[continued] tail "+head) {
			t.Fatal("concurrent calls interleaved")
		}
	}
}

func TestOneLineEscapesAreReversible(t *testing.T) {
	input := "fake \\n versus newline\n and \r and \x1b plus é"
	decoded, err := strconv.Unquote("\"" + strings.ReplaceAll(oneLine(input), "\"", "\\\"") + "\"")
	if err != nil || decoded != input {
		t.Fatal("literal escapes cannot be distinguished from controls")
	}
}
