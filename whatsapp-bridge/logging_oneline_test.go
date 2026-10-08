package main

// In the text log format nothing a stanza carries can start a second line or
// drive the terminal (issue #492).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
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
		"João da Silva — São Paulo",
		"family 👨‍👩‍👧 group",       // zero-width joiners inside an emoji sequence
		"שלום עולם, مرحبا بالعالم", // right-to-left scripts, no bidi control
		"日本語のグループ",
		"a\ttabbed\tline",
		`a Windows path C:\store\messages.db and a literal \n`,
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
		"a\nb":                     `a\nb`,
		"a\r\nb":                   `a\r\nb`,
		"a\x00b":                   `a\x00b`,
		"red \x1b[31malert\x1b[0m": `red \x1b[31malert\x1b[0m`, // a terminal escape sequence
		"a\x7fb":                   `a\x7fb`,
		"a\u0085b":                 `a\u0085b`, // NEL, the C1 "next line"
		"a\u2028b\u2029c":          `a\u2028b\u2029c`,
		"user\u202egpj.exe":        `user\u202egpj.exe`, // right-to-left override
		"a\u2066b\u2069":           `a\u2066b\u2069`,
		"bad \xff byte":            `bad \xff byte`,
		"é\nü":                     `é\nü`, // the text around an escape is kept as it is
		forgedName:                 `x\n12:00:00.000 [Bridge ERROR] the store was wiped`,
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

const (
	textLoggerHelperEnv = "WAMCP_TEXT_LOGGER_HELPER"
	// The child prints these around its log calls, so whatever else a test
	// binary writes to stdout (TestMain, the PASS line) is not counted.
	textLoggerBegin = "=== text logger output begins"
	textLoggerEnd   = "=== text logger output ends"
)

// TestTextLoggerHelperProcess is not a test of its own: it is the child
// TestTextLoggerPrintsOneLinePerCall starts, so the real text loggers write to
// a real stdout that the parent reads, without swapping os.Stdout under the
// other tests of the package.
func TestTextLoggerHelperProcess(t *testing.T) {
	if os.Getenv(textLoggerHelperEnv) != "1" {
		return
	}
	fmt.Println(textLoggerBegin)
	defer fmt.Println(textLoggerEnd)
	bridge, client, db := newLoggerSet("INFO", false)
	bridge.Infof("Updating chat name from PushName for %s: %s -> %s", "sender@lid", "Old Name", forgedName)
	bridge.Warnf("Media URL expired for %s (%v); requesting media retry", forgedID, "403")
	client.Sub("Socket").Warnf("unexpected frame from %s", forgedName)
	db.Errorf("%s", forgedName)
	bridge.Debugf("below the level, never printed: %s", forgedName)
	bridge.Infof("an ordinary name: %s", "João 👨‍👩‍👧 שלום")
}

var textLogLine = regexp.MustCompile(`^\d\d:\d\d:\d\d\.\d{3} \[(Bridge|Client/Socket|Database) (INFO|WARN|ERROR)\] `)

func TestTextLoggerPrintsOneLinePerCall(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestTextLoggerHelperProcess$") //nolint:gosec // the test binary re-running one of its own tests
	cmd.Env = append(os.Environ(), textLoggerHelperEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	_, text, begun := strings.Cut(text, textLoggerBegin+"\n")
	text, _, ended := strings.Cut(text, textLoggerEnd+"\n")
	if !begun || !ended {
		t.Fatalf("the helper did not print both markers:\n%s", out)
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("five calls at or above INFO must print five lines, got %d:\n%s", len(lines), out)
	}
	for i, line := range lines {
		if !textLogLine.MatchString(line) {
			t.Errorf("line %d does not start like a log line of the logger that printed it: %q", i, line)
		}
	}
	for i, want := range []string{
		`[Bridge INFO] Updating chat name from PushName for sender@lid: Old Name -> x\n12:00:00.000 [Bridge ERROR] the store was wiped`,
		`[Bridge WARN] Media URL expired for 3EB0\r\n12:00:00.000 [Bridge INFO] paired a new device (403); requesting media retry`,
		`[Client/Socket WARN] unexpected frame from x\n12:00:00.000`,
		`[Database ERROR] x\n12:00:00.000`,
		`[Bridge INFO] an ordinary name: João 👨‍👩‍👧 שלום`,
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want it to contain %q", i, lines[i], want)
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
	ordinary := "Jo" + string(rune(0xE3)) + "o " + string(rune(0x200F))
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

// The level is checked before the message is formatted, and a sub-logger keeps
// both the level and the escaping.
func TestOneLineLoggerHonoursTheLevelAndWrapsSubLoggers(t *testing.T) {
	rec := &recordingLogger{}
	l := oneLineLogger{inner: rec, min: levelRank["WARN"]}
	l.Debugf("d %s", forgedName)
	l.Infof("i %s", forgedName)
	l.Warnf("w %s", forgedName)
	l.Sub("Socket").Errorf("e %s", forgedName)
	want := "[WARN] w " + oneLine(forgedName) + "\n[ERROR] e " + oneLine(forgedName) + "\n"
	if got := rec.String(); got != want {
		t.Errorf("recorded %q, want %q", got, want)
	}
}
