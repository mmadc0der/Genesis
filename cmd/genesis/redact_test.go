package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactorValueRedactsNestedJSON(t *testing.T) {
	secret := "sk-live-secret-value"
	red := newRedactor([]string{secret})
	got := red.value(map[string]any{
		"hint": "used " + secret,
		"ok":   true,
	})
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(secret)) {
		t.Fatalf("secret remained in diagnostics value: %s", payload)
	}
	if !bytes.Contains(payload, []byte(redactedSecret)) {
		t.Fatalf("redacted marker missing: %s", payload)
	}
}

func TestRedactorValueLeavesNilAndUnknownUnchanged(t *testing.T) {
	red := newRedactor([]string{"sk-live-secret-value"})
	if got := red.value(nil); got != nil {
		t.Fatalf("nil diagnostics = %v", got)
	}
	if got := (*redactor)(nil).value("plain"); got != "plain" {
		t.Fatalf("nil redactor = %v", got)
	}
	unchanged := map[string]any{"hint": "no secret here"}
	got := red.value(unchanged)
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "no secret here") {
		t.Fatalf("unrelated diagnostics rewritten: %s", payload)
	}
}

func TestCopyCappedThenDrainDiscardsRemainder(t *testing.T) {
	const limit = 8
	src := bytes.NewReader(append(bytes.Repeat([]byte("a"), limit), []byte("OVERFLOW")...))
	var retained bytes.Buffer
	copyCappedThenDrain(&retained, src, limit)
	if retained.String() != strings.Repeat("a", limit) {
		t.Fatalf("retained %q", retained.String())
	}
	if src.Len() != 0 {
		t.Fatalf("remainder not drained, leftover %d", src.Len())
	}
}
