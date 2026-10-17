package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func testFileReference() ports.ChatFileReference {
	n := 3
	return ports.ChatFileReference{ID: "selection", SessionID: "session", Path: "src/retry.ts", Source: ports.ChatFileReferenceSource{Kind: "workspace"}, Side: "new", Line: 3, EndLine: 3, Lines: []ports.ChatFileReferenceLine{{Kind: "add", NewNo: &n, Text: "await retry();"}}}
}

func TestFileReferenceValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ports.ChatFileReference)
	}{
		{"absolute", func(r *ports.ChatFileReference) { r.Path = "C:/secret" }},
		{"traversal", func(r *ports.ChatFileReference) { r.Path = "../secret" }},
		{"backslash", func(r *ports.ChatFileReference) { r.Path = "a\\b" }},
		{"source", func(r *ports.ChatFileReference) { r.Source.Kind = "url" }},
		{"pr", func(r *ports.ChatFileReference) { r.Source.Kind = "pull_request" }},
		{"range", func(r *ports.ChatFileReference) { r.EndLine = 2 }},
		{"row size", func(r *ports.ChatFileReference) { r.Lines[0].Text = strings.Repeat("x", 701) }},
		{"newline", func(r *ports.ChatFileReference) { r.Lines[0].Text = "a\nb" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testFileReference()
			tc.mutate(&r)
			if ValidateFileReference(r) == nil {
				t.Fatal("accepted invalid reference")
			}
		})
	}
	if err := ValidateFileReference(testFileReference()); err != nil {
		t.Fatal(err)
	}
}

func TestFileReferenceDeliveryKeepsDurableSnapshotAndExpandsOnce(t *testing.T) {
	ref := testFileReference()
	msg := ports.ChatUserMessage{Text: "why?", Content: []ports.ChatContent{{Type: "file_reference", FileReference: &ref}}}
	before, _ := json.Marshal(msg)
	delivered := excerptDeliveryMessage(msg)
	if len(delivered.Content) != 0 || strings.Count(delivered.Text, "await retry();") != 1 || !strings.Contains(delivered.Text, "src/retry.ts#L3-L3") {
		t.Fatalf("delivery: %+v", delivered)
	}
	after, _ := json.Marshal(msg)
	if string(before) != string(after) {
		t.Fatal("mutated durable message")
	}
	retry, err := retryPromptContent(string(mustFileContentJSON(t, msg.Content)), nil)
	if err != nil || len(retry) != 1 || retry[0].FileReference.ID != ref.ID {
		t.Fatalf("retry lost snapshot: %+v %v", retry, err)
	}
}
func mustFileContentJSON(t *testing.T, content []ports.ChatContent) []byte {
	t.Helper()
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFileReferenceOwnershipAndCombinedLimit(t *testing.T) {
	ref := testFileReference()
	msg := ports.ChatUserMessage{Content: []ports.ChatContent{{Type: "file_reference", FileReference: &ref}}}
	if validateFileReferenceContent(msg, "other") == nil {
		t.Fatal("cross-session reference accepted")
	}
	msg.Excerpts = make([]ports.ChatExcerptReference, 8)
	if validateFileReferenceContent(msg, "session") == nil {
		t.Fatal("combined limit ignored")
	}
}

func TestMixedAnnotationReceiptPreservesCapturedFileContent(t *testing.T) {
	ref := testFileReference()
	content := []ports.ChatContent{{Type: "file_reference", FileReference: &ref}}
	raw := string(mustFileContentJSON(t, content))
	if !fileReferenceContentsMatch(raw, content) {
		t.Fatal("same selection refused")
	}
	if fileReferenceContentsMatch(raw, nil) {
		t.Fatal("dropped selection accepted")
	}
	ref.Lines[0].Text = "replacement();"
	if fileReferenceContentsMatch(raw, content) {
		t.Fatal("changed captured code accepted")
	}
}

func TestFileReferenceValidationAcceptsIndependentDiffSideCoordinates(t *testing.T) {
	ref := testFileReference()
	oldLine, newLine := 10, 9
	ref.Side, ref.EndSide, ref.Line, ref.EndLine = "old", "new", oldLine, newLine
	ref.Lines = []ports.ChatFileReferenceLine{{Kind: "del", Side: "old", OldNo: &oldLine, Text: "removed();"}, {Kind: "context", Side: "new", NewNo: &newLine, Text: "context();"}}
	if err := ValidateFileReference(ref); err != nil {
		t.Fatalf("valid mixed-side range rejected: %v", err)
	}
	ref.Lines[1].Side, ref.Lines[1].OldNo = "old", &newLine
	ref.EndSide = "old"
	if err := ValidateFileReference(ref); err == nil {
		t.Fatal("accepted reversed range on the same side")
	}
}
