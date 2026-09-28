// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package hclsyntax

import (
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
)

// ByteEdit replaces a single non-overlapping byte range within a source
// buffer. The range is half-open: StartByte is inclusive and EndByte is
// exclusive, using the same byte offsets as hcl.Pos.
type ByteEdit struct {
	StartByte int
	EndByte   int
	NewText   string
}

// RenameRootVariable edits describe the source-level edits required to rename
// a root variable within an HCL native-syntax configuration.
//
// Only root references are rewritten: references inside "for" expression
// local scopes, object constructor literal keys, ordinary string literals,
// comments, block labels, and attribute names are left untouched.
// Interpolations within string templates are handled because they contain
// real expressions.
//
// The returned edits are sorted by position and never overlap, so they can be
// applied independently or via ApplyByteEdits. The rest of the source buffer,
// including whitespace, comments, and line endings such as CRLF, is preserved
// byte-for-byte; no reformatting is performed.
//
// If the source contains syntax errors, or if the requested rename would
// change the meaning of a reference because the new name is masked by a local
// variable at that position, no edits are returned and the returned
// diagnostics describe the problem. No partial replacement is ever produced.
func RenameRootVariable(src []byte, filename string, oldName, newName string) ([]ByteEdit, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	if !ValidIdentifier(oldName) {
		return nil, diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid variable name",
			Detail:   fmt.Sprintf("The old variable name %q is not a valid HCL identifier.", oldName),
		})
	}
	if !ValidIdentifier(newName) {
		return nil, diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid variable name",
			Detail:   fmt.Sprintf("The new variable name %q is not a valid HCL identifier.", newName),
		})
	}
	if oldName == newName {
		return nil, nil
	}

	file, parseDiags := ParseConfig(src, filename, hcl.InitialPos)
	diags = append(diags, parseDiags...)
	if diags.HasErrors() {
		// The partially-constructed AST produced after a parse error is not
		// trustworthy, so refuse rather than applying a partial rewrite.
		return nil, diags
	}

	walker := &renameWalker{
		oldName: oldName,
		newName: newName,
	}
	walkDiags := Walk(file.Body.(*Body), walker)
	diags = append(diags, walkDiags...)
	if diags.HasErrors() {
		return nil, diags
	}

	sort.Slice(walker.edits, func(i, j int) bool {
		return walker.edits[i].StartByte < walker.edits[j].StartByte
	})

	// Defensive overlap check. Each TraverseRoot describes a distinct token,
	// so overlaps would indicate an internal bug rather than user input.
	for i := 1; i < len(walker.edits); i++ {
		if walker.edits[i].StartByte < walker.edits[i-1].EndByte {
			panic("hclsyntax: overlapping root variable edits")
		}
	}

	return walker.edits, nil
}

// ApplyByteEdits returns a copy of src with all of the given edits applied.
//
// Edits must be sorted by StartByte, non-empty in their covered range, and
// non-overlapping, as guaranteed by RenameRootVariable. The output is built
// using byte offsets, so UTF-8 content outside of the edited ranges and line
// endings are preserved exactly.
func ApplyByteEdits(src []byte, edits []ByteEdit) []byte {
	var out []byte
	prev := 0
	for _, edit := range edits {
		out = append(out, src[prev:edit.StartByte]...)
		out = append(out, edit.NewText...)
		prev = edit.EndByte
	}
	out = append(out, src[prev:]...)
	return out
}

type renameWalker struct {
	oldName     string
	newName     string
	localScopes []map[string]struct{}
	edits       []ByteEdit
}

func (w *renameWalker) Enter(node Node) hcl.Diagnostics {
	switch n := node.(type) {
	case *ScopeTraversalExpr:
		root, ok := n.Traversal[0].(hcl.TraverseRoot)
		if !ok || root.Name != w.oldName {
			return nil
		}
		for _, locals := range w.localScopes {
			if _, localized := locals[w.oldName]; localized {
				// The reference resolves to a "for" expression local rather
				// than the root variable, so it is left untouched.
				return nil
			}
			if _, masked := locals[w.newName]; masked {
				return hcl.Diagnostics{
					&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Variable name is masked by a local variable",
						Detail: fmt.Sprintf(
							"Renaming %q to %q here would change the reference's meaning because %q is declared by an enclosing for expression.",
							w.oldName, w.newName, w.newName,
						),
						Subject: &root.SrcRange,
					},
				}
			}
		}
		w.edits = append(w.edits, ByteEdit{
			StartByte: root.SrcRange.Start.Byte,
			EndByte:   root.SrcRange.End.Byte,
			NewText:   w.newName,
		})
	case ChildScope:
		w.localScopes = append(w.localScopes, n.LocalNames)
	}
	return nil
}

func (w *renameWalker) Exit(node Node) hcl.Diagnostics {
	if _, ok := node.(ChildScope); ok {
		w.localScopes = w.localScopes[:len(w.localScopes)-1]
	}
	return nil
}
