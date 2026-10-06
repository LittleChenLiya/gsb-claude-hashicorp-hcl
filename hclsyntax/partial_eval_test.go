// Copyright IBM Corp. 2014, 2025
// SPDX-License-Identifier: MPL-2.0

package hclsyntax

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

func parseTestExpr(t *testing.T, src string) Expression {
	t.Helper()
	expr, diags := ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("failed to parse %q: %s", src, diags.Error())
	}
	return expr
}

func literalVal(t *testing.T, expr Expression) cty.Value {
	t.Helper()
	lit, ok := expr.(*LiteralValueExpr)
	if !ok {
		t.Fatalf("residual is %T, want *LiteralValueExpr", expr)
	}
	return lit.Val
}

func traversalStrings(travs []hcl.Traversal) []string {
	var ret []string
	for _, tr := range travs {
		ret = append(ret, traversalKey(tr))
	}
	return ret
}

func assertDeps(t *testing.T, deps []hcl.Traversal, want ...string) {
	t.Helper()
	got := traversalStrings(deps)
	if len(got) != len(want) {
		t.Fatalf("deps = %v, want %v", got, want)
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for _, g := range got {
		if !wantSet[g] {
			t.Fatalf("deps = %v, want %v", got, want)
		}
	}
}

func TestPartialEvalConditionalKnownFalse(t *testing.T) {
	expr := parseTestExpr(t, `enabled ? secret.value : 3`)
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"enabled": cty.BoolVal(false),
		},
	}
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	val := literalVal(t, residual)
	if !val.RawEquals(cty.NumberIntVal(3)) {
		t.Fatalf("got %#v, want 3", val)
	}
	assertDeps(t, deps)
}

func TestPartialEvalConditionalKnownTrue(t *testing.T) {
	expr := parseTestExpr(t, `enabled ? secret.value : 3`)
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"enabled": cty.BoolVal(true),
			"secret": cty.ObjectVal(map[string]cty.Value{
				"value": cty.StringVal("s3cr3t"),
			}),
		},
	}
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	val := literalVal(t, residual)
	if !val.RawEquals(cty.StringVal("s3cr3t")) {
		t.Fatalf("got %#v, want s3cr3t", val)
	}
	assertDeps(t, deps)
}

func TestPartialEvalConditionalUnknownCondKeepsBothBranches(t *testing.T) {
	expr := parseTestExpr(t, `enabled ? secret.value : 3`)
	residual, deps, diags := PartialEval(expr, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	cond, ok := residual.(*ConditionalExpr)
	if !ok {
		t.Fatalf("residual is %T, want *ConditionalExpr", residual)
	}
	// The known false branch should still have been folded to a literal.
	if _, ok := cond.FalseResult.(*LiteralValueExpr); !ok {
		t.Fatalf("false branch is %T, want *LiteralValueExpr", cond.FalseResult)
	}
	assertDeps(t, deps, "enabled", "secret.value")
}

func TestPartialEvalShortCircuit(t *testing.T) {
	tests := []struct {
		src   string
		known map[string]cty.Value
		want  cty.Value
		deps  []string
	}{
		{`enabled && secret.value`, map[string]cty.Value{"enabled": cty.BoolVal(false)}, cty.BoolVal(false), nil},
		{`enabled || secret.value`, map[string]cty.Value{"enabled": cty.BoolVal(true)}, cty.BoolVal(true), nil},
		{`unknown_flag && false`, nil, cty.BoolVal(false), nil},
		{`unknown_flag || true`, nil, cty.BoolVal(true), nil},
	}
	for _, test := range tests {
		t.Run(test.src, func(t *testing.T) {
			expr := parseTestExpr(t, test.src)
			known := &hcl.EvalContext{Variables: test.known}
			residual, deps, diags := PartialEval(expr, known)
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Error())
			}
			val := literalVal(t, residual)
			if !val.RawEquals(test.want) {
				t.Fatalf("got %#v, want %#v", val, test.want)
			}
			assertDeps(t, deps, test.deps...)
		})
	}
}

func TestPartialEvalShortCircuitKeepsUnknownOperand(t *testing.T) {
	expr := parseTestExpr(t, `true && enabled`)
	residual, deps, diags := PartialEval(expr, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if _, ok := residual.(*ScopeTraversalExpr); !ok {
		t.Fatalf("residual is %T, want *ScopeTraversalExpr", residual)
	}
	assertDeps(t, deps, "enabled")
}

func TestPartialEvalTemplateInterpolation(t *testing.T) {
	expr := parseTestExpr(t, `"prefix-${name}-suffix"`)
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"name": cty.StringVal("VALUE"),
		},
	}
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	val := literalVal(t, residual)
	if !val.RawEquals(cty.StringVal("prefix-VALUE-suffix")) {
		t.Fatalf("got %#v", val)
	}
	assertDeps(t, deps)

	// With an unknown variable the template remains, with known parts folded.
	residual, deps, diags = PartialEval(expr, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if _, ok := residual.(*TemplateExpr); !ok {
		t.Fatalf("residual is %T, want *TemplateExpr", residual)
	}
	assertDeps(t, deps, "name")
}

func TestPartialEvalForExpression(t *testing.T) {
	// Fully-known for expression folds to a literal.
	expr := parseTestExpr(t, `[for v in xs : v * 2]`)
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"xs": cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2)}),
		},
	}
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	val := literalVal(t, residual)
	want := cty.TupleVal([]cty.Value{cty.NumberIntVal(2), cty.NumberIntVal(4)})
	if !val.RawEquals(want) {
		t.Fatalf("got %#v, want %#v", val, want)
	}
	assertDeps(t, deps)

	// Unknown collection: the for expression remains, with its body folded.
	expr = parseTestExpr(t, `[for v in xs : "${v}-${suffix}"]`)
	known = &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"suffix": cty.StringVal("S"),
		},
	}
	residual, deps, diags = PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if _, ok := residual.(*ForExpr); !ok {
		t.Fatalf("residual is %T, want *ForExpr", residual)
	}
	assertDeps(t, deps, "xs")
}

func TestPartialEvalForExpressionLocalShadowing(t *testing.T) {
	// A known variable named like the local symbol must not be substituted.
	expr := parseTestExpr(t, `[for v in [1, 2] : v]`)
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"v": cty.NumberIntVal(100),
		},
	}
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	val := literalVal(t, residual)
	want := cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2)})
	if !val.RawEquals(want) {
		t.Fatalf("got %#v, want %#v", val, want)
	}
	assertDeps(t, deps)
}

func TestPartialEvalIndexOutOfRange(t *testing.T) {
	expr := parseTestExpr(t, `[1, 2][5]`)
	residual, _, diags := PartialEval(expr, nil)
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics for out-of-range index, got none")
	}
	if diags[0].Subject == nil {
		t.Fatal("diagnostic has no subject range")
	}
	if diags[0].Subject.Filename != "test.hcl" {
		t.Fatalf("diagnostic range filename = %q, want test.hcl", diags[0].Subject.Filename)
	}
	// The failing expression is left in place as the residual.
	switch residual.(type) {
	case *IndexExpr, *RelativeTraversalExpr:
	default:
		t.Fatalf("residual is %T, want *IndexExpr or *RelativeTraversalExpr", residual)
	}
}

func TestPartialEvalSensitiveMarks(t *testing.T) {
	expr := parseTestExpr(t, `enabled ? secret : "fallback"`)
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"enabled": cty.BoolVal(true),
			"secret":  cty.StringVal("s3cr3t").Mark("sensitive"),
		},
	}
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	val := literalVal(t, residual)
	if !val.IsMarked() {
		t.Fatal("folded value lost its marks")
	}
	uv, marks := val.Unmark()
	if _, ok := marks["sensitive"]; !ok {
		t.Fatalf("marks = %#v, want sensitive", marks)
	}
	if !uv.RawEquals(cty.StringVal("s3cr3t")) {
		t.Fatalf("got %#v", uv)
	}
	assertDeps(t, deps)

	// Marks also survive folding through templates and collections.
	expr = parseTestExpr(t, `["${secret}"]`)
	residual, _, diags = PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	val = literalVal(t, residual)
	if !val.RawEquals(cty.TupleVal([]cty.Value{cty.StringVal("s3cr3t").Mark("sensitive")})) {
		t.Fatalf("got %#v, marks not preserved", val)
	}
}

func TestPartialEvalNeverCallsFunctions(t *testing.T) {
	called := false
	sentinel := function.New(&function.Spec{
		Params: []function.Parameter{{Name: "v", Type: cty.Number}},
		Type:   function.StaticReturnType(cty.Number),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			called = true
			return args[0], nil
		},
	})
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"n": cty.NumberIntVal(2),
		},
		Functions: map[string]function.Function{"sentinel": sentinel},
	}

	// Arguments are folded, but the call itself must remain and must not
	// have been executed.
	expr := parseTestExpr(t, `sentinel(1 + n)`)
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	call, ok := residual.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("residual is %T, want *FunctionCallExpr", residual)
	}
	if called {
		t.Fatal("sentinel function was called during partial evaluation")
	}
	arg := literalVal(t, call.Args[0])
	if !arg.RawEquals(cty.NumberIntVal(3)) {
		t.Fatalf("call argument = %#v, want 3", arg)
	}
	assertDeps(t, deps)

	// A pure operator whose operand contains a call must not fold either.
	expr = parseTestExpr(t, `1 + sentinel(2)`)
	residual, _, diags = PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if _, ok := residual.(*BinaryOpExpr); !ok {
		t.Fatalf("residual is %T, want *BinaryOpExpr", residual)
	}
	if called {
		t.Fatal("sentinel function was called during partial evaluation")
	}
}

func TestPartialEvalResidualSemantics(t *testing.T) {
	// Evaluating the residual expression in a context completed with the
	// remaining dependencies must give the same result as evaluating the
	// original expression in the complete context.
	tests := []struct {
		src   string
		known map[string]cty.Value
		rest  map[string]cty.Value
	}{
		{
			`enabled ? secret.value : 3`,
			map[string]cty.Value{"enabled": cty.BoolVal(false)},
			map[string]cty.Value{"secret": cty.ObjectVal(map[string]cty.Value{"value": cty.NumberIntVal(7)})},
		},
		{
			`enabled ? secret.value : 3`,
			nil,
			map[string]cty.Value{
				"enabled": cty.BoolVal(true),
				"secret":  cty.ObjectVal(map[string]cty.Value{"value": cty.NumberIntVal(7)}),
			},
		},
		{
			`{a = 1 + 2, b = name, c = [for v in xs : v * n]}`,
			map[string]cty.Value{"n": cty.NumberIntVal(10)},
			map[string]cty.Value{
				"name": cty.StringVal("x"),
				"xs":   cty.TupleVal([]cty.Value{cty.NumberIntVal(1)}),
			},
		},
		{
			`"${greeting}, ${name}!"`,
			map[string]cty.Value{"greeting": cty.StringVal("hi")},
			map[string]cty.Value{"name": cty.StringVal("bob")},
		},
		{
			`flag && count > 2 || fallback`,
			map[string]cty.Value{"count": cty.NumberIntVal(5)},
			map[string]cty.Value{
				"flag":     cty.BoolVal(true),
				"fallback": cty.BoolVal(false),
			},
		},
		{
			`items[0].id`,
			nil,
			map[string]cty.Value{
				"items": cty.TupleVal([]cty.Value{
					cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("a")}),
				}),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.src, func(t *testing.T) {
			expr := parseTestExpr(t, test.src)

			fullVars := map[string]cty.Value{}
			for k, v := range test.known {
				fullVars[k] = v
			}
			for k, v := range test.rest {
				fullVars[k] = v
			}
			fullCtx := &hcl.EvalContext{Variables: fullVars}

			want, diags := expr.Value(fullCtx)
			if diags.HasErrors() {
				t.Fatalf("original expression failed: %s", diags.Error())
			}

			residual, deps, diags := PartialEval(expr, &hcl.EvalContext{Variables: test.known})
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Error())
			}

			// Every remaining dependency must be satisfiable from the
			// "rest" variables only.
			for _, dep := range deps {
				if _, ok := test.known[dep.RootName()]; ok {
					t.Fatalf("dependency %s was already known", traversalKey(dep))
				}
				if _, ok := test.rest[dep.RootName()]; !ok {
					t.Fatalf("dependency %s not satisfiable from rest", traversalKey(dep))
				}
			}

			got, diags := residual.Value(fullCtx)
			if diags.HasErrors() {
				t.Fatalf("residual expression failed: %s", diags.Error())
			}
			if !got.RawEquals(want) {
				t.Fatalf("residual evaluates to %#v, want %#v", got, want)
			}
		})
	}
}

func TestPartialEvalPureLiteralOps(t *testing.T) {
	tests := []struct {
		src  string
		want cty.Value
	}{
		{`1 + 2 * 3`, cty.NumberIntVal(7)},
		{`!false`, cty.BoolVal(true)},
		{`-5`, cty.NumberIntVal(-5)},
		{`[1, "two", true]`, cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.StringVal("two"), cty.BoolVal(true)})},
		{`{a = 1, b = "x"}`, cty.ObjectVal(map[string]cty.Value{"a": cty.NumberIntVal(1), "b": cty.StringVal("x")})},
		{`{for k, v in {x = 1} : k => v if v > 0}`, cty.ObjectVal(map[string]cty.Value{"x": cty.NumberIntVal(1)})},
		{`["a", "b"][1]`, cty.StringVal("b")},
		{`{a = {b = 42}}.a.b`, cty.NumberIntVal(42)},
		{`[{id = "a"}, {id = "b"}][*].id`, cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})},
	}
	for _, test := range tests {
		t.Run(test.src, func(t *testing.T) {
			expr := parseTestExpr(t, test.src)
			residual, deps, diags := PartialEval(expr, nil)
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Error())
			}
			val := literalVal(t, residual)
			if !val.RawEquals(test.want) {
				t.Fatalf("got %#v, want %#v", val, test.want)
			}
			assertDeps(t, deps)
		})
	}
}

func TestPartialEvalUnknownVariableValueRemainsDependency(t *testing.T) {
	expr := parseTestExpr(t, `maybe + 1`)
	known := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"maybe": cty.UnknownVal(cty.Number),
		},
	}
	residual, deps, diags := PartialEval(expr, known)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}
	if _, ok := residual.(*BinaryOpExpr); !ok {
		t.Fatalf("residual is %T, want *BinaryOpExpr", residual)
	}
	assertDeps(t, deps, "maybe")
}
