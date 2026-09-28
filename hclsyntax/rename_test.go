// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package hclsyntax

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

func rootNamesOf(expr Expression) []string {
	var names []string
	for _, traversal := range Variables(expr) {
		names = append(names, traversal.RootName())
	}
	return names
}

func applyRename(t *testing.T, src, oldName, newName string) []byte {
	t.Helper()
	edits, diags := RenameRootVariable([]byte(src), "test.hcl", oldName, newName)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	return ApplyByteEdits([]byte(src), edits)
}

// The acceptance case: a root reference, an object literal key, and bare
// occurrences inside strings and comments all share the spelling var_old,
// but only the real root reference is rewritten.
func TestRenameRootVariableOnlyRootReferences(t *testing.T) {
	src := strings.Join([]string{
		`# var_old stays in this comment`,
		`attr = var_old.name`,
		`obj = {`,
		`  var_old = "var_old"  # var_old`,
		`  (var_old) = var_old.id`,
		`  interpolated = "prefix-${var_old.name}-var_old"`,
		`}`,
	}, "\r\n")

	got := applyRename(t, src, "var_old", "var_new")
	want := strings.Join([]string{
		`# var_old stays in this comment`,
		`attr = var_new.name`,
		`obj = {`,
		`  var_old = "var_old"  # var_old`,
		`  (var_new) = var_new.id`,
		`  interpolated = "prefix-${var_new.name}-var_old"`,
		`}`,
	}, "\r\n")
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("wrong result\n got: %q\nwant: %q", got, want)
	}
}

func TestRenameRootVariableForLocalScope(t *testing.T) {
	// The collection expression is evaluated outside the for scope. The key
	// and value expressions see the local variables x and item.
	src := `out = [for x, item in var_old.list: "${item.thing}-${var_old.other}" if x < var_old.limit]`
	got := applyRename(t, src, "var_old", "var_new")
	want := `out = [for x, item in var_new.list: "${item.thing}-${var_new.other}" if x < var_new.limit]`
	if string(got) != want {
		t.Fatalf("wrong result\n got: %q\nwant: %q", got, want)
	}

	// A reference spelled like the old name but resolving to a for-local is
	// not a root reference and must be left alone.
	shadowed := `out = [for var_old in items: var_old.value]`
	got = applyRename(t, shadowed, "var_old", "var_new")
	if string(got) != shadowed {
		t.Fatalf("local reference was rewritten: %q", got)
	}
}

func TestRenameRootVariableMaskedNewNameRejected(t *testing.T) {
	src := `out = [for var_new, item in items: { a = var_old.one, b = var_old.two }]`
	edits, diags := RenameRootVariable([]byte(src), "test.hcl", "var_old", "var_new")
	if !diags.HasErrors() {
		t.Fatalf("expected error, got edits %#v", edits)
	}
	if edits != nil {
		t.Fatalf("expected no edits on rejection, got %#v", edits)
	}
	// Both conflicting references must be reported so no single rewrite is
	// silently dropped.
	if len(diags) != 2 {
		t.Fatalf("wrong diagnostic count %d: %s", len(diags), diags)
	}
	for _, diag := range diags {
		subject := diag.Subject
		if subject == nil || string(src[subject.Start.Byte:subject.End.Byte]) != "var_old" {
			t.Fatalf("diagnostic should point at a var_old reference: %#v", diag)
		}
	}
}

func TestRenameRootVariableNestedTemplates(t *testing.T) {
	src := `a = "%{if var_old.enabled}${[for v in var_old.items: "${v}-${var_old.suffix}"]}%{endif}"`
	got := applyRename(t, src, "var_old", "var_new")
	want := `a = "%{if var_new.enabled}${[for v in var_new.items: "${v}-${var_new.suffix}"]}%{endif}"`
	if string(got) != want {
		t.Fatalf("wrong result\n got: %q\nwant: %q", got, want)
	}

	// A for-local must mask the new name even inside a nested template,
	// making the rename unsafe at the var_old reference in the value.
	shadowed := `a = "${[for var_new in items: "${var_old}"]}"`
	edits, diags := RenameRootVariable([]byte(shadowed), "test.hcl", "var_old", "var_new")
	if !diags.HasErrors() {
		t.Fatalf("expected masking error, got %#v", edits)
	}
}

func TestRenameRootVariableUnicode(t *testing.T) {
	src := `值 = 旧名.字段` + "\n" + `other = "旧名"`
	got := applyRename(t, src, "旧名", "新名")
	want := `值 = 新名.字段` + "\n" + `other = "旧名"`
	if string(got) != want {
		t.Fatalf("wrong result\n got: %q\nwant: %q", got, want)
	}
}

// Bytes outside of the identifier ranges, including CRLF line endings and
// comments, must remain untouched.
func TestRenameRootVariablePreservesBytesOutsideEdits(t *testing.T) {
	src := []byte("# var_old comment\r\n" +
		"a = var_old.x // trailing var_old\r\n" +
		"b = {\"var_old\" = var_old}\r\n")
	edits, diags := RenameRootVariable(src, "test.hcl", "var_old", "var_new")
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if len(edits) != 2 {
		t.Fatalf("wrong edit count %d: %#v", len(edits), edits)
	}
	out := ApplyByteEdits(src, edits)
	if bytes.Count(out, []byte("\r\n")) != 3 {
		t.Fatalf("CRLF line endings were not preserved: %q", out)
	}
	want := []byte("# var_old comment\r\n" +
		"a = var_new.x // trailing var_old\r\n" +
		"b = {\"var_old\" = var_new}\r\n")
	if !bytes.Equal(out, want) {
		t.Fatalf("wrong result\n got: %q\nwant: %q", out, want)
	}
}

func TestRenameRootVariableNoMatch(t *testing.T) {
	src := []byte("a = something.else\n")
	edits, diags := RenameRootVariable(src, "test.hcl", "var_old", "var_new")
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if len(edits) != 0 {
		t.Fatalf("expected no edits, got %#v", edits)
	}
	out := ApplyByteEdits(src, edits)
	if !bytes.Equal(out, src) {
		t.Fatalf("source changed: %q", out)
	}

	// Renaming a name to itself is also a no-op.
	edits, diags = RenameRootVariable([]byte("a = var_old.x"), "test.hcl", "var_old", "var_old")
	if diags.HasErrors() || len(edits) != 0 {
		t.Fatalf("same-name rename should be a no-op: %#v %s", edits, diags)
	}
}

func TestRenameRootVariableSyntaxErrorNoPartialEdits(t *testing.T) {
	src := []byte("a = var_old.x\nb = var_old.\n")
	edits, diags := RenameRootVariable(src, "test.hcl", "var_old", "var_new")
	if !diags.HasErrors() {
		t.Fatal("expected syntax error diagnostics")
	}
	if edits != nil {
		t.Fatalf("syntax errors must not produce edits, got %#v", edits)
	}
}

func TestRenameRootVariableInvalidIdentifier(t *testing.T) {
	if _, diags := RenameRootVariable(nil, "test.hcl", "-bad", "b"); !diags.HasErrors() {
		t.Fatal("expected error for invalid old name")
	}
	if _, diags := RenameRootVariable(nil, "test.hcl", "a", "1bad"); !diags.HasErrors() {
		t.Fatal("expected error for invalid new name")
	}
}

func TestRenameRootVariableEditsSortedAndNonOverlapping(t *testing.T) {
	src := []byte("a = var_old\nb = \"x=${var_old.x},y=${var_old.y}\"\n")
	edits, diags := RenameRootVariable(src, "test.hcl", "var_old", "var_new")
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if len(edits) != 3 {
		t.Fatalf("wrong edit count %d", len(edits))
	}
	for i, edit := range edits {
		if i > 0 && edit.StartByte <= edits[i-1].StartByte {
			t.Fatalf("edits not sorted: %#v", edits)
		}
		if i > 0 && edit.StartByte < edits[i-1].EndByte {
			t.Fatalf("overlapping edits: %#v", edits)
		}
		if string(src[edit.StartByte:edit.EndByte]) != "var_old" {
			t.Fatalf("edit does not cover old identifier: %#v", edit)
		}
	}
}

// After applying the edits the file must parse again and expose the new
// name (and only the new name) through Variables.
func TestRenameRootVariableReparseReferences(t *testing.T) {
	src := strings.Join([]string{
		`a = var_old.x`,
		`b = {var_old = true, real = var_old.y}`,
		`c = "var_old ${var_old.z}"`,
		`d = [for var_old in var_old.items: var_old]`,
	}, "\n")

	edits, diags := RenameRootVariable([]byte(src), "test.hcl", "var_old", "var_new")
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	out := ApplyByteEdits([]byte(src), edits)

	file, parseDiags := ParseConfig(out, "test.hcl", hcl.InitialPos)
	if parseDiags.HasErrors() {
		t.Fatalf("renamed source does not parse: %s", parseDiags.Error())
	}
	body := file.Body.(*Body)
	attrs, _ := body.JustAttributes()

	cases := map[string][]string{
		"a": {"var_new"},
		"b": {"var_new"}, // literal key contributes no root reference
		"c": {"var_new"}, // bare text inside the string is inert
		// Collection is renamed to var_new; the for-local var_old in the
		// value remains a local rather than a root variable.
		"d": {"var_new"},
	}
	for name, want := range cases {
		got := rootNamesOf(attrs[name].Expr.(Expression))
		if len(got) != len(want) {
			t.Fatalf("attr %s: wrong roots %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("attr %s: wrong root %q, want %q", name, got[i], want[i])
			}
		}
	}
}
