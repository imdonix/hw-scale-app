package logx

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "info")
	l.Debug("hidden")
	l.Info("MEASUREMENT", "weight", "86.25kg", "note", "two words")
	out := b.String()
	if strings.Contains(out, "hidden") {
		t.Fatal("debug line printed at info level")
	}
	if !strings.Contains(out, " INFO  MEASUREMENT weight=86.25kg note=\"two words\"\n") {
		t.Fatalf("unexpected format: %q", out)
	}
}
