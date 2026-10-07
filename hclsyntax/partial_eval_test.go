// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package hclsyntax

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

func parsePartialEvalTestExpr(t *testing.T, src string) Expression {
	t.Helper()
	expr, diags := ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("failed to parse expression %q: %s", src, diags.Error())
	}
	return expr
}

func traversalStrings(traversals []hcl.Traversal) []string {
	var ret []string
	for _, traversal := range traversals {
		ret = append(ret, traversalKey(traversal))
	}
	return ret
}

func assertLiteralValue(t *testing.T, expr Expression, want cty.Value) {
	t.Helper()
	lit, ok := expr.(*LiteralValueExpr)
	if !ok {
		t.Fatalf("residual is %T; want *LiteralValueExpr", expr)
	}
	if !lit.Val.RawEquals(want) {
		t.Fatalf("wrong value\ngot:  %#v\nwant: %#v", lit.Val, want)
	}
}

func TestPartialEvalConditionalKnownFalse(t *testing.T) {
	expr := parsePartialEvalTestExpr(t, `enabled ? secret.value : 3`)
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"enabled": cty.False,
		},
	}

	residual, vars, diags := PartialEval(expr, ctx)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	assertLiteralValue(t, residual, cty.NumberIntVal(3))
	if len(vars) != 0 {
		t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
	}
}

func TestPartialEvalConditionalKnownTrue(t *testing.T) {
	expr := parsePartialEvalTestExpr(t, `enabled ? secret.value : 3`)
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"enabled": cty.True,
		},
	}

	residual, vars, diags := PartialEval(expr, ctx)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if _, ok := residual.(*ScopeTraversalExpr); !ok {
		t.Fatalf("residual is %T; want *ScopeTraversalExpr", residual)
	}
	if got, want := traversalStrings(vars), []string{"root:secret;attr:value;"}; !equalStrings(got, want) {
		t.Fatalf("wrong remaining variables %v; want %v", got, want)
	}

	// Completing the evaluation with the remaining variable must produce
	// the same result as evaluating the original expression.
	full := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"enabled": cty.True,
			"secret":  cty.ObjectVal(map[string]cty.Value{"value": cty.StringVal("s3cr3t")}),
		},
	}
	gotVal, gotDiags := residual.Value(full)
	if gotDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics evaluating residual: %s", gotDiags.Error())
	}
	wantVal, wantDiags := expr.Value(full)
	if wantDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics evaluating original: %s", wantDiags.Error())
	}
	if !gotVal.RawEquals(wantVal) {
		t.Fatalf("residual evaluates to %#v; want %#v", gotVal, wantVal)
	}
}

func TestPartialEvalConditionalUnknownCondition(t *testing.T) {
	expr := parsePartialEvalTestExpr(t, `enabled ? secret.value : 3`)

	residual, vars, diags := PartialEval(expr, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	cond, ok := residual.(*ConditionalExpr)
	if !ok {
		t.Fatalf("residual is %T; want *ConditionalExpr", residual)
	}
	// Both branches must be retained.
	if _, ok := cond.TrueResult.(*ScopeTraversalExpr); !ok {
		t.Fatalf("true branch is %T; want *ScopeTraversalExpr", cond.TrueResult)
	}
	assertLiteralValue(t, cond.FalseResult, cty.NumberIntVal(3))
	if got, want := traversalStrings(vars), []string{"root:enabled;", "root:secret;attr:value;"}; !equalStrings(got, want) {
		t.Fatalf("wrong remaining variables %v; want %v", got, want)
	}

	// The residual must evaluate like the original once the remaining
	// variables are supplied, for either condition outcome.
	for _, enabled := range []cty.Value{cty.True, cty.False} {
		full := &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"enabled": enabled,
				"secret":  cty.ObjectVal(map[string]cty.Value{"value": cty.StringVal("s3cr3t")}),
			},
		}
		gotVal, gotDiags := residual.Value(full)
		if gotDiags.HasErrors() {
			t.Fatalf("unexpected diagnostics evaluating residual: %s", gotDiags.Error())
		}
		wantVal, wantDiags := expr.Value(full)
		if wantDiags.HasErrors() {
			t.Fatalf("unexpected diagnostics evaluating original: %s", wantDiags.Error())
		}
		if !gotVal.RawEquals(wantVal) {
			t.Fatalf("with enabled=%#v residual evaluates to %#v; want %#v", enabled, gotVal, wantVal)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPartialEvalPureLiteralOperations(t *testing.T) {
	tests := []struct {
		src  string
		want cty.Value
	}{
		{`1 + 2 * 3`, cty.NumberIntVal(7)},
		{`(1 + 2) * 3`, cty.NumberIntVal(9)},
		{`!false`, cty.True},
		{`true && false`, cty.False},
		{`true || false`, cty.True},
		{`1 == 1`, cty.True},
		{`"foo" == "bar"`, cty.False},
		{`-5`, cty.NumberIntVal(-5)},
		{`[1, 2, 3]`, cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2), cty.NumberIntVal(3)})},
		{`{ a = 1, b = "x" }`, cty.ObjectVal(map[string]cty.Value{"a": cty.NumberIntVal(1), "b": cty.StringVal("x")})},
		{`[1, 2, 3][1]`, cty.NumberIntVal(2)},
		{`{ a = 1 }.a`, cty.NumberIntVal(1)},
		{`"hello ${"wor"}ld"`, cty.StringVal("hello world")},
		{`"${1 + 1}"`, cty.NumberIntVal(2)},
		{`true ? "yes" : "no"`, cty.StringVal("yes")},
	}

	for _, test := range tests {
		t.Run(test.src, func(t *testing.T) {
			expr := parsePartialEvalTestExpr(t, test.src)
			residual, vars, diags := PartialEval(expr, nil)
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Error())
			}
			assertLiteralValue(t, residual, test.want)
			if len(vars) != 0 {
				t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
			}
		})
	}
}

func TestPartialEvalKnownVariableFolding(t *testing.T) {
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"num":  cty.NumberIntVal(4),
			"list": cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
			"obj":  cty.ObjectVal(map[string]cty.Value{"attr": cty.StringVal("val")}),
		},
	}

	tests := []struct {
		src  string
		want cty.Value
	}{
		{`num * 2`, cty.NumberIntVal(8)},
		{`num + 1 > 4`, cty.True},
		{`list[0]`, cty.StringVal("a")},
		{`obj.attr`, cty.StringVal("val")},
		{`length_placeholder`, cty.NilVal}, // placeholder, replaced below
	}
	tests = tests[:4]

	for _, test := range tests {
		t.Run(test.src, func(t *testing.T) {
			expr := parsePartialEvalTestExpr(t, test.src)
			residual, vars, diags := PartialEval(expr, ctx)
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Error())
			}
			assertLiteralValue(t, residual, test.want)
			if len(vars) != 0 {
				t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
			}
		})
	}
}

func TestPartialEvalForExpression(t *testing.T) {
	t.Run("known collection folds", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `[for v in [1, 2, 3] : v * 2]`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		assertLiteralValue(t, residual, cty.TupleVal([]cty.Value{
			cty.NumberIntVal(2), cty.NumberIntVal(4), cty.NumberIntVal(6),
		}))
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("known collection and known outer variable folds", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `[for v in [1, 2] : v + offset]`)
		ctx := &hcl.EvalContext{
			Variables: map[string]cty.Value{"offset": cty.NumberIntVal(10)},
		}
		residual, vars, diags := PartialEval(expr, ctx)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		assertLiteralValue(t, residual, cty.TupleVal([]cty.Value{
			cty.NumberIntVal(11), cty.NumberIntVal(12),
		}))
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("unknown collection is retained", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `[for v in nums : v * 2 if v > 1]`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if _, ok := residual.(*ForExpr); !ok {
			t.Fatalf("residual is %T; want *ForExpr", residual)
		}
		if got, want := traversalStrings(vars), []string{"root:nums;"}; !equalStrings(got, want) {
			t.Fatalf("wrong remaining variables %v; want %v", got, want)
		}

		full := &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"nums": cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2), cty.NumberIntVal(3)}),
			},
		}
		gotVal, gotDiags := residual.Value(full)
		if gotDiags.HasErrors() {
			t.Fatalf("unexpected diagnostics evaluating residual: %s", gotDiags.Error())
		}
		wantVal, wantDiags := expr.Value(full)
		if wantDiags.HasErrors() {
			t.Fatalf("unexpected diagnostics evaluating original: %s", wantDiags.Error())
		}
		if !gotVal.RawEquals(wantVal) {
			t.Fatalf("residual evaluates to %#v; want %#v", gotVal, wantVal)
		}
	})

	t.Run("object for expression folds", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `{for k, v in { a = 1, b = 2 } : k => v + 1}`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		assertLiteralValue(t, residual, cty.ObjectVal(map[string]cty.Value{
			"a": cty.NumberIntVal(2),
			"b": cty.NumberIntVal(3),
		}))
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})
}

func TestPartialEvalTemplateInterpolation(t *testing.T) {
	t.Run("unknown interpolation is retained", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `"hello ${name}!"`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if _, ok := residual.(*TemplateExpr); !ok {
			t.Fatalf("residual is %T; want *TemplateExpr", residual)
		}
		if got, want := traversalStrings(vars), []string{"root:name;"}; !equalStrings(got, want) {
			t.Fatalf("wrong remaining variables %v; want %v", got, want)
		}

		full := &hcl.EvalContext{
			Variables: map[string]cty.Value{"name": cty.StringVal("world")},
		}
		gotVal, gotDiags := residual.Value(full)
		if gotDiags.HasErrors() {
			t.Fatalf("unexpected diagnostics evaluating residual: %s", gotDiags.Error())
		}
		if !gotVal.RawEquals(cty.StringVal("hello world!")) {
			t.Fatalf("residual evaluates to %#v; want %#v", gotVal, cty.StringVal("hello world!"))
		}
	})

	t.Run("known interpolation folds", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `"hello ${name}!"`)
		ctx := &hcl.EvalContext{
			Variables: map[string]cty.Value{"name": cty.StringVal("world")},
		}
		residual, vars, diags := PartialEval(expr, ctx)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		assertLiteralValue(t, residual, cty.StringVal("hello world!"))
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("mixed known and unknown parts", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `"${1 + 1}-${suffix}"`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if _, ok := residual.(*TemplateExpr); !ok {
			t.Fatalf("residual is %T; want *TemplateExpr", residual)
		}
		if got, want := traversalStrings(vars), []string{"root:suffix;"}; !equalStrings(got, want) {
			t.Fatalf("wrong remaining variables %v; want %v", got, want)
		}

		full := &hcl.EvalContext{
			Variables: map[string]cty.Value{"suffix": cty.StringVal("end")},
		}
		gotVal, gotDiags := residual.Value(full)
		if gotDiags.HasErrors() {
			t.Fatalf("unexpected diagnostics evaluating residual: %s", gotDiags.Error())
		}
		if !gotVal.RawEquals(cty.StringVal("2-end")) {
			t.Fatalf("residual evaluates to %#v; want %#v", gotVal, cty.StringVal("2-end"))
		}
	})
}

func TestPartialEvalIndexOutOfBounds(t *testing.T) {
	expr := parsePartialEvalTestExpr(t, `[1, 2][5]`)

	residual, vars, diags := PartialEval(expr, nil)
	if !diags.HasErrors() {
		t.Fatalf("expected an error diagnostic, got none")
	}
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics; want exactly 1 (no duplicates)", len(diags))
	}
	diag := diags[0]
	if diag.Severity != hcl.DiagError {
		t.Fatalf("diagnostic severity is %v; want error", diag.Severity)
	}
	if diag.Subject == nil {
		t.Fatalf("diagnostic has no source range")
	}
	if got, want := diag.Subject.Start.Byte, 6; got != want {
		t.Fatalf("diagnostic range starts at byte %d; want %d", got, want)
	}
	// The failing expression is retained as the residual so the error can
	// still be reproduced at final evaluation time.
	if _, ok := residual.(*RelativeTraversalExpr); !ok {
		t.Fatalf("residual is %T; want *RelativeTraversalExpr", residual)
	}
	if len(vars) != 0 {
		t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
	}
}

func TestPartialEvalIndexUnknownCollection(t *testing.T) {
	expr := parsePartialEvalTestExpr(t, `var.list[0]`)

	residual, vars, diags := PartialEval(expr, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if _, ok := residual.(*ScopeTraversalExpr); !ok {
		t.Fatalf("residual is %T; want *ScopeTraversalExpr", residual)
	}
	if len(vars) != 1 || vars[0].RootName() != "var" {
		t.Fatalf("wrong remaining variables %v; want a single traversal rooted at var", traversalStrings(vars))
	}
}

func TestPartialEvalSensitiveMarks(t *testing.T) {
	sensitiveVal := cty.StringVal("hunter2").Mark("sensitive")
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"password": sensitiveVal,
			"count":    cty.NumberIntVal(2).Mark("sensitive"),
		},
	}

	t.Run("marks survive folding through a template", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `"pw=${password}"`)
		residual, vars, diags := PartialEval(expr, ctx)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
		lit, ok := residual.(*LiteralValueExpr)
		if !ok {
			t.Fatalf("residual is %T; want *LiteralValueExpr", residual)
		}
		if !lit.Val.HasMark("sensitive") {
			t.Fatalf("folded value %#v lost its sensitive mark", lit.Val)
		}
		if unmarked, _ := lit.Val.Unmark(); !unmarked.RawEquals(cty.StringVal("pw=hunter2")) {
			t.Fatalf("folded value is %#v; want %#v", lit.Val, cty.StringVal("pw=hunter2"))
		}
	})

	t.Run("marks survive folding through arithmetic", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `count + 1`)
		residual, _, diags := PartialEval(expr, ctx)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		lit, ok := residual.(*LiteralValueExpr)
		if !ok {
			t.Fatalf("residual is %T; want *LiteralValueExpr", residual)
		}
		if !lit.Val.HasMark("sensitive") {
			t.Fatalf("folded value %#v lost its sensitive mark", lit.Val)
		}
		if unmarked, _ := lit.Val.Unmark(); !unmarked.RawEquals(cty.NumberIntVal(3)) {
			t.Fatalf("folded value is %#v; want %#v", lit.Val, cty.NumberIntVal(3))
		}
	})

	t.Run("marks survive conditional reduction", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `cond ? password : "none"`)
		condCtx := &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"cond":     cty.True,
				"password": sensitiveVal,
			},
		}
		residual, vars, diags := PartialEval(expr, condCtx)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
		lit, ok := residual.(*LiteralValueExpr)
		if !ok {
			t.Fatalf("residual is %T; want *LiteralValueExpr", residual)
		}
		if !lit.Val.HasMark("sensitive") {
			t.Fatalf("folded value %#v lost its sensitive mark", lit.Val)
		}
	})
}

func TestPartialEvalFunctionCallsNeverExecuted(t *testing.T) {
	called := 0
	sentinel := function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "val", Type: cty.DynamicPseudoType},
		},
		Type: function.StaticReturnType(cty.DynamicPseudoType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			called++
			return args[0], nil
		},
	})
	newCtx := func() *hcl.EvalContext {
		return &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"enabled": cty.False,
			},
			Functions: map[string]function.Function{
				"sentinel": sentinel,
			},
		}
	}

	t.Run("call with foldable arguments is retained but reduced", func(t *testing.T) {
		called = 0
		expr := parsePartialEvalTestExpr(t, `sentinel(1 + 1)`)
		residual, vars, diags := PartialEval(expr, newCtx())
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if called != 0 {
			t.Fatalf("sentinel function was executed %d times during partial evaluation", called)
		}
		call, ok := residual.(*FunctionCallExpr)
		if !ok {
			t.Fatalf("residual is %T; want *FunctionCallExpr", residual)
		}
		if len(call.Args) != 1 {
			t.Fatalf("residual call has %d args; want 1", len(call.Args))
		}
		assertLiteralValue(t, call.Args[0], cty.NumberIntVal(2))
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}

		// The residual call must still be executable at final evaluation.
		val, valDiags := residual.Value(newCtx())
		if valDiags.HasErrors() {
			t.Fatalf("unexpected diagnostics evaluating residual: %s", valDiags.Error())
		}
		if called != 1 {
			t.Fatalf("sentinel was executed %d times at final evaluation; want 1", called)
		}
		if !val.RawEquals(cty.NumberIntVal(2)) {
			t.Fatalf("residual call evaluates to %#v; want %#v", val, cty.NumberIntVal(2))
		}
	})

	t.Run("call in unreachable branch is dropped without executing", func(t *testing.T) {
		called = 0
		expr := parsePartialEvalTestExpr(t, `enabled ? sentinel(secret) : 3`)
		residual, vars, diags := PartialEval(expr, newCtx())
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if called != 0 {
			t.Fatalf("sentinel function was executed %d times during partial evaluation", called)
		}
		assertLiteralValue(t, residual, cty.NumberIntVal(3))
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("call inside for body prevents folding but is not executed", func(t *testing.T) {
		called = 0
		expr := parsePartialEvalTestExpr(t, `[for v in [1, 2] : sentinel(v)]`)
		residual, vars, diags := PartialEval(expr, newCtx())
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if called != 0 {
			t.Fatalf("sentinel function was executed %d times during partial evaluation", called)
		}
		if _, ok := residual.(*ForExpr); !ok {
			t.Fatalf("residual is %T; want *ForExpr", residual)
		}
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("unregistered function is retained without diagnostics", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `nosuchfunc(1)`)
		residual, _, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if _, ok := residual.(*FunctionCallExpr); !ok {
			t.Fatalf("residual is %T; want *FunctionCallExpr", residual)
		}
	})
}

// TestPartialEvalResidualSemantics verifies that for a variety of
// expressions, evaluating the residual expression with the remaining
// variables supplied produces the same value as evaluating the original
// expression with all variables supplied.
func TestPartialEvalResidualSemantics(t *testing.T) {
	known := map[string]cty.Value{
		"offset": cty.NumberIntVal(10),
		"prefix": cty.StringVal("pre"),
		"flag":   cty.True,
	}
	unknown := map[string]cty.Value{
		"a":    cty.NumberIntVal(3),
		"b":    cty.StringVal("bee"),
		"c":    cty.StringVal("see"),
		"cond": cty.False,
		"list": cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2)}),
		"obj":  cty.ObjectVal(map[string]cty.Value{"attr": cty.StringVal("val")}),
	}

	tests := []string{
		`a + 1 * 2`,
		`a * offset`,
		`a > 1 ? "big" : "small"`,
		`flag ? a : offset`,
		`cond ? b : c`,
		`flag ? "fallback" : missing.deeply.nested`,
		`[for v in list : v + offset]`,
		`[for v in list : v + a]`,
		`[for v in [1, 2] : v * a]`,
		`"${prefix}-${a}"`,
		`"${prefix}-${list[0]}"`,
		`{ x = a * 2, y = cond ? b : c }`,
		`{ x = offset, y = a }`,
		`list[1]`,
		`list[a - 3]`,
		`obj.attr`,
		`(a + offset) * 2`,
		`!flag`,
		`flag && cond`,
		`a == offset`,
	}

	for _, src := range tests {
		t.Run(src, func(t *testing.T) {
			expr := parsePartialEvalTestExpr(t, src)

			knownCtx := &hcl.EvalContext{Variables: known}
			residual, vars, diags := PartialEval(expr, knownCtx)
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Error())
			}

			// Every remaining dependency must be one of the unknown
			// variables, and every unknown variable the residual needs
			// must appear in the dependency list.
			for _, traversal := range vars {
				root := traversal.RootName()
				if _, isKnown := known[root]; isKnown {
					t.Fatalf("known variable %q reported as a remaining dependency", root)
				}
				if _, isUnknown := unknown[root]; !isUnknown {
					t.Fatalf("unexpected remaining dependency %q", root)
				}
			}

			fullVars := map[string]cty.Value{}
			for k, v := range known {
				fullVars[k] = v
			}
			for k, v := range unknown {
				fullVars[k] = v
			}
			fullCtx := &hcl.EvalContext{Variables: fullVars}

			gotVal, gotDiags := residual.Value(fullCtx)
			if gotDiags.HasErrors() {
				t.Fatalf("unexpected diagnostics evaluating residual: %s", gotDiags.Error())
			}
			wantVal, wantDiags := expr.Value(fullCtx)
			if wantDiags.HasErrors() {
				t.Fatalf("unexpected diagnostics evaluating original: %s", wantDiags.Error())
			}
			if !gotVal.RawEquals(wantVal) {
				t.Fatalf("residual evaluates to %#v; original evaluates to %#v", gotVal, wantVal)
			}
		})
	}
}

func TestPartialEvalNullCondition(t *testing.T) {
	expr := parsePartialEvalTestExpr(t, `null ? 1 : 2`)

	_, _, diags := PartialEval(expr, nil)
	if !diags.HasErrors() {
		t.Fatalf("expected an error diagnostic, got none")
	}
	if got, want := diags[0].Summary, "Null condition"; got != want {
		t.Fatalf("wrong diagnostic summary %q; want %q", got, want)
	}
	if diags[0].Subject == nil {
		t.Fatalf("diagnostic has no source range")
	}
}

func TestPartialEvalInvalidTraversalOnKnownVariable(t *testing.T) {
	expr := parsePartialEvalTestExpr(t, `obj.nosuchattr`)
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"obj": cty.ObjectVal(map[string]cty.Value{"attr": cty.StringVal("val")}),
		},
	}

	_, vars, diags := PartialEval(expr, ctx)
	if !diags.HasErrors() {
		t.Fatalf("expected an error diagnostic, got none")
	}
	if diags[0].Subject == nil {
		t.Fatalf("diagnostic has no source range")
	}
	if len(vars) != 0 {
		t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
	}
}

func TestPartialEvalObjectKeyNotTreatedAsVariable(t *testing.T) {
	// A naked identifier key in an object constructor is a literal string,
	// not a reference, even if a variable of the same name is known.
	expr := parsePartialEvalTestExpr(t, `{ foo = 1 }`)
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"foo": cty.StringVal("wrong"),
		},
	}

	residual, vars, diags := PartialEval(expr, ctx)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	assertLiteralValue(t, residual, cty.ObjectVal(map[string]cty.Value{
		"foo": cty.NumberIntVal(1),
	}))
	if len(vars) != 0 {
		t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
	}
}

func TestPartialEvalShortCircuitLogicalOperators(t *testing.T) {
	// HCL's && and || short-circuit: when the left operand decides the
	// result, the right operand is never evaluated, so its references
	// must not be reported as remaining dependencies.
	t.Run("false && anything is false", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `false && missing`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		assertLiteralValue(t, residual, cty.False)
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("true || anything is true", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `true || missing`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		assertLiteralValue(t, residual, cty.True)
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("non-deciding left operand keeps the right dependency", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `true && missing`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if _, ok := residual.(*BinaryOpExpr); !ok {
			t.Fatalf("residual is %T; want *BinaryOpExpr", residual)
		}
		if got, want := traversalStrings(vars), []string{"root:missing;"}; !equalStrings(got, want) {
			t.Fatalf("wrong remaining variables %v; want %v", got, want)
		}
	})
}

func TestPartialEvalUnreachableBranchDepsNotReported(t *testing.T) {
	// When the condition is decidable, variables that appear only in the
	// unreachable branch must not be reported as remaining dependencies,
	// even when they are not present in the known set.
	expr := parsePartialEvalTestExpr(t, `true ? "kept" : gone.nested.deep`)

	residual, vars, diags := PartialEval(expr, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	assertLiteralValue(t, residual, cty.StringVal("kept"))
	if len(vars) != 0 {
		t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
	}
}

func TestPartialEvalForLocalShadowsKnownVariable(t *testing.T) {
	// The iteration variable of a for expression shadows any known
	// variable of the same name, so it must not be substituted.
	expr := parsePartialEvalTestExpr(t, `[for offset in [1, 2] : offset + 1]`)
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"offset": cty.NumberIntVal(100),
		},
	}

	residual, vars, diags := PartialEval(expr, ctx)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	assertLiteralValue(t, residual, cty.TupleVal([]cty.Value{
		cty.NumberIntVal(2), cty.NumberIntVal(3),
	}))
	if len(vars) != 0 {
		t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
	}
}

func TestPartialEvalForBodyUsesKnownOuterVariable(t *testing.T) {
	// A known variable referenced from a for body is substituted even
	// though the collection is unknown.
	expr := parsePartialEvalTestExpr(t, `[for v in nums : v + offset]`)
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"offset": cty.NumberIntVal(10),
		},
	}

	residual, vars, diags := PartialEval(expr, ctx)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	forExpr, ok := residual.(*ForExpr)
	if !ok {
		t.Fatalf("residual is %T; want *ForExpr", residual)
	}
	// The body must have been reduced to v + 10.
	body, ok := forExpr.ValExpr.(*BinaryOpExpr)
	if !ok {
		t.Fatalf("residual body is %T; want *BinaryOpExpr", forExpr.ValExpr)
	}
	assertLiteralValue(t, body.RHS, cty.NumberIntVal(10))
	// Only the unknown collection remains as a dependency.
	if got, want := traversalStrings(vars), []string{"root:nums;"}; !equalStrings(got, want) {
		t.Fatalf("wrong remaining variables %v; want %v", got, want)
	}

	full := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"offset": cty.NumberIntVal(10),
			"nums":   cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2)}),
		},
	}
	gotVal, gotDiags := residual.Value(full)
	if gotDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics evaluating residual: %s", gotDiags.Error())
	}
	wantVal, wantDiags := expr.Value(full)
	if wantDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics evaluating original: %s", wantDiags.Error())
	}
	if !gotVal.RawEquals(wantVal) {
		t.Fatalf("residual evaluates to %#v; want %#v", gotVal, wantVal)
	}
}

func TestPartialEvalSplatExpression(t *testing.T) {
	t.Run("known source folds", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `objs[*].a`)
		ctx := &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"objs": cty.TupleVal([]cty.Value{
					cty.ObjectVal(map[string]cty.Value{"a": cty.NumberIntVal(1)}),
					cty.ObjectVal(map[string]cty.Value{"a": cty.NumberIntVal(2)}),
				}),
			},
		}
		residual, vars, diags := PartialEval(expr, ctx)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		assertLiteralValue(t, residual, cty.TupleVal([]cty.Value{
			cty.NumberIntVal(1), cty.NumberIntVal(2),
		}))
		if len(vars) != 0 {
			t.Fatalf("wrong remaining variables %v; want none", traversalStrings(vars))
		}
	})

	t.Run("unknown source is retained", func(t *testing.T) {
		expr := parsePartialEvalTestExpr(t, `objs[*].a`)
		residual, vars, diags := PartialEval(expr, nil)
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Error())
		}
		if _, ok := residual.(*SplatExpr); !ok {
			t.Fatalf("residual is %T; want *SplatExpr", residual)
		}
		if got, want := len(vars), 1; got != want {
			t.Fatalf("got %d remaining variables; want %d", got, want)
		}
		if got, want := vars[0].RootName(), "objs"; got != want {
			t.Fatalf("remaining variable root is %q; want %q", got, want)
		}
	})
}
