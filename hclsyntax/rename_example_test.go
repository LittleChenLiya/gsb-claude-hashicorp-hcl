// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package hclsyntax_test

import (
	"fmt"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ExampleRenameRootVariable shows the byte ranges selected for a rename and
// the source after applying them. Literal object keys and text inside
// ordinary strings or comments are preserved.
func ExampleRenameRootVariable() {
	src := []byte("a = var_old.name\n" +
		"b = {var_old = \"var_old\"}\n" +
		"# var_old\n")

	edits, diags := hclsyntax.RenameRootVariable(src, "example.hcl", "var_old", "var_new")
	if diags.HasErrors() {
		fmt.Printf("error: %s\n", diags)
		return
	}
	for _, edit := range edits {
		fmt.Printf("[%d:%d] %s -> %s\n",
			edit.StartByte, edit.EndByte,
			src[edit.StartByte:edit.EndByte], edit.NewText)
	}
	fmt.Printf("%s", hclsyntax.ApplyByteEdits(src, edits))
	// Output:
	// [4:11] var_old -> var_new
	// a = var_new.name
	// b = {var_old = "var_old"}
	// # var_old
}

