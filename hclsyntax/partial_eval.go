// Copyright IBM Corp. 2014, 2025
// SPDX-License-Identifier: MPL-2.0

package hclsyntax

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// PartialEval performs static partial evaluation of the given expression
// using the variable values available in the given evaluation context.
//
// Any sub-expression whose operands are all statically known is folded into
// a literal value. Conditionals and the short-circuiting logical operators
// ("&&" and "||") whose outcomes are statically determined are replaced by
// the branch that will actually be taken, so that variables referenced only
// by an unreachable branch do not become dependencies of the result. When a
// condition is not statically known, both arms of the conditional are
// preserved in the residual expression.
//
// Function calls are never executed, even if the called function is
// registered in the given context: a call always remains in the residual
// expression, with only its arguments partially evaluated.
//
// The return values are the residual expression, the traversals that the
// residual expression still depends on (i.e. those not resolvable to a
// wholly-known value in the given context), and any diagnostics produced
// while folding. Diagnostics always carry a source range. Evaluating the
// residual expression in a context that additionally provides values for
// the remaining dependencies produces the same result as evaluating the
// original expression in a complete context.
//
// Variables whose values are themselves unknown (cty.UnknownVal etc.) are
// treated as not statically known and remain as dependencies.
func PartialEval(expr Expression, known *hcl.EvalContext) (Expression, []hcl.Traversal, hcl.Diagnostics) {
	p := &partialEvaluator{known: known}
	residual, _, _ := p.eval(expr)
	deps := p.remainingDeps(residual)
	return residual, deps, p.dedupeDiags()
}

// partialEvaluator holds the state for a single partial evaluation.
type partialEvaluator struct {
	known *hcl.EvalContext

	// shadowed counts how many enclosing local scopes (for expressions)
	// bind each root name, so that a known variable is not substituted
	// for a reference to a local symbol that happens to share its name.
	shadowed map[string]int

	diags hcl.Diagnostics
}

// eval partially evaluates expr, returning the residual expression. If the
// expression is entirely statically known, the residual is a
// *LiteralValueExpr and the returned value and boolean describe the
// statically-known result.
func (p *partialEvaluator) eval(expr Expression) (Expression, cty.Value, bool) {
	switch e := expr.(type) {
	case *LiteralValueExpr:
		if e.Val.IsWhollyKnown() {
			return expr, e.Val, true
		}
		return expr, cty.NilVal, false

	case *ScopeTraversalExpr:
		if val, ok := p.lookup(e.Traversal); ok {
			return &LiteralValueExpr{Val: val, SrcRange: e.Range()}, val, true
		}
		return expr, cty.NilVal, false

	case *ParenthesesExpr:
		inner, val, ok := p.eval(e.Expression)
		if !ok {
			return &ParenthesesExpr{Expression: inner, SrcRange: e.SrcRange}, cty.NilVal, false
		}
		return &LiteralValueExpr{Val: val, SrcRange: e.SrcRange}, val, true

	case *TemplateExpr:
		parts := make([]Expression, len(e.Parts))
		for i, part := range e.Parts {
			parts[i], _, _ = p.eval(part)
		}
		return p.tryStatic(&TemplateExpr{Parts: parts, SrcRange: e.SrcRange})

	case *TemplateWrapExpr:
		wrapped, _, _ := p.eval(e.Wrapped)
		return p.tryStatic(&TemplateWrapExpr{Wrapped: wrapped, SrcRange: e.SrcRange})

	case *TemplateJoinExpr:
		tuple, _, _ := p.eval(e.Tuple)
		return p.tryStatic(&TemplateJoinExpr{Tuple: tuple})

	case *TupleConsExpr:
		elems := make([]Expression, len(e.Exprs))
		for i, elem := range e.Exprs {
			elems[i], _, _ = p.eval(elem)
		}
		return p.tryStatic(&TupleConsExpr{
			Exprs:     elems,
			SrcRange:  e.SrcRange,
			OpenRange: e.OpenRange,
		})

	case *ObjectConsExpr:
		items := make([]ObjectConsItem, len(e.Items))
		for i, item := range e.Items {
			key, _, _ := p.eval(item.KeyExpr)
			val, _, _ := p.eval(item.ValueExpr)
			items[i] = ObjectConsItem{KeyExpr: key, ValueExpr: val}
		}
		return p.tryStatic(&ObjectConsExpr{
			Items:     items,
			SrcRange:  e.SrcRange,
			OpenRange: e.OpenRange,
		})

	case *ObjectConsKeyExpr:
		// A bare, traversal-shaped key that is not forced to be evaluated
		// is a literal string, not a variable reference.
		if !e.ForceNonLiteral {
			if name := e.literalName(); name != "" {
				return expr, cty.StringVal(name), true
			}
		}
		wrapped, val, ok := p.eval(e.Wrapped)
		if !ok {
			return &ObjectConsKeyExpr{Wrapped: wrapped, ForceNonLiteral: true}, cty.NilVal, false
		}
		return &LiteralValueExpr{Val: val, SrcRange: e.Range()}, val, true

	case *UnaryOpExpr:
		operand, _, _ := p.eval(e.Val)
		return p.tryStatic(&UnaryOpExpr{
			Op:          e.Op,
			Val:         operand,
			SrcRange:    e.SrcRange,
			SymbolRange: e.SymbolRange,
		})

	case *BinaryOpExpr:
		if e.Op == OpLogicalAnd || e.Op == OpLogicalOr {
			if residual, val, ok, done := p.evalLogical(e); done {
				return residual, val, ok
			}
		}
		lhs, _, _ := p.eval(e.LHS)
		rhs, _, _ := p.eval(e.RHS)
		return p.tryStatic(&BinaryOpExpr{
			LHS:      lhs,
			Op:       e.Op,
			RHS:      rhs,
			SrcRange: e.SrcRange,
		})

	case *ConditionalExpr:
		condExpr, condVal, condKnown := p.eval(e.Condition)
		if condKnown {
			uv, marks := condVal.Unmark()
			if uv.IsNull() {
				p.diags = append(p.diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Null condition",
					Detail:   "The condition value is null. Conditions must either be true or false.",
					Subject:  condExpr.Range().Ptr(),
					Context:  &e.SrcRange,
				})
			} else if cv, err := convert.Convert(uv, cty.Bool); err != nil {
				p.diags = append(p.diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Incorrect condition type",
					Detail:   "The condition expression must be of type bool.",
					Subject:  condExpr.Range().Ptr(),
					Context:  &e.SrcRange,
				})
			} else {
				// The condition is statically known, so only the taken
				// branch contributes dependencies to the residual
				// expression; the other branch is discarded.
				trueExpr, trueVal, trueOK := p.eval(e.TrueResult)
				falseExpr, falseVal, falseOK := p.eval(e.FalseResult)
				if trueOK && falseOK {
					// If both branches are statically known, evaluate
					// the whole conditional so that the result type
					// unification and mark handling exactly match
					// normal evaluation.
					static := &ConditionalExpr{
						Condition:   &LiteralValueExpr{Val: condVal, SrcRange: condExpr.Range()},
						TrueResult:  &LiteralValueExpr{Val: trueVal, SrcRange: trueExpr.Range()},
						FalseResult: &LiteralValueExpr{Val: falseVal, SrcRange: falseExpr.Range()},
						SrcRange:    e.SrcRange,
					}
					val, diags := static.Value(&hcl.EvalContext{})
					p.diags = append(p.diags, diags...)
					if !diags.HasErrors() && val.IsWhollyKnown() {
						return &LiteralValueExpr{Val: val, SrcRange: e.SrcRange}, val, true
					}
				}
				taken, takenVal, takenOK := trueExpr, trueVal, trueOK
				if cv.False() {
					taken, takenVal, takenOK = falseExpr, falseVal, falseOK
				}
				if takenOK {
					val := takenVal.WithMarks(marks)
					return &LiteralValueExpr{Val: val, SrcRange: e.SrcRange}, val, true
				}
				return taken, cty.NilVal, false
			}
		}
		// The condition is not statically known, so both arms must be
		// preserved in the residual expression.
		trueExpr, _, _ := p.eval(e.TrueResult)
		falseExpr, _, _ := p.eval(e.FalseResult)
		return &ConditionalExpr{
			Condition:   condExpr,
			TrueResult:  trueExpr,
			FalseResult: falseExpr,
			SrcRange:    e.SrcRange,
		}, cty.NilVal, false

	case *IndexExpr:
		collection, _, _ := p.eval(e.Collection)
		key, _, _ := p.eval(e.Key)
		return p.tryStatic(&IndexExpr{
			Collection:   collection,
			Key:          key,
			SrcRange:     e.SrcRange,
			OpenRange:    e.OpenRange,
			BracketRange: e.BracketRange,
		})

	case *RelativeTraversalExpr:
		source, _, _ := p.eval(e.Source)
		return p.tryStatic(&RelativeTraversalExpr{
			Source:    source,
			Traversal: e.Traversal,
			SrcRange:  e.SrcRange,
		})

	case *FunctionCallExpr:
		// Function calls are never executed during partial evaluation,
		// even if the function is registered in the evaluation context.
		// Only the arguments are partially evaluated.
		args := make([]Expression, len(e.Args))
		for i, arg := range e.Args {
			args[i], _, _ = p.eval(arg)
		}
		return &FunctionCallExpr{
			Name:            e.Name,
			Args:            args,
			ExpandFinal:     e.ExpandFinal,
			NameRange:       e.NameRange,
			OpenParenRange:  e.OpenParenRange,
			CloseParenRange: e.CloseParenRange,
		}, cty.NilVal, false

	case *ForExpr:
		coll, _, _ := p.eval(e.CollExpr)
		p.shadow(e.KeyVar)
		p.shadow(e.ValVar)
		var keyExpr, condExpr Expression
		if e.KeyExpr != nil {
			keyExpr, _, _ = p.eval(e.KeyExpr)
		}
		valExpr, _, _ := p.eval(e.ValExpr)
		if e.CondExpr != nil {
			condExpr, _, _ = p.eval(e.CondExpr)
		}
		p.unshadow(e.KeyVar)
		p.unshadow(e.ValVar)
		return p.tryStatic(&ForExpr{
			KeyVar:     e.KeyVar,
			ValVar:     e.ValVar,
			CollExpr:   coll,
			KeyExpr:    keyExpr,
			ValExpr:    valExpr,
			CondExpr:   condExpr,
			Group:      e.Group,
			SrcRange:   e.SrcRange,
			OpenRange:  e.OpenRange,
			CloseRange: e.CloseRange,
		})

	case *SplatExpr:
		source, _, _ := p.eval(e.Source)
		each, _, _ := p.eval(e.Each)
		return p.tryStatic(&SplatExpr{
			Source:      source,
			Each:        each,
			Item:        e.Item,
			SrcRange:    e.SrcRange,
			MarkerRange: e.MarkerRange,
		})

	default:
		// Includes *AnonSymbolExpr (a local symbol of an enclosing for or
		// splat expression) and *ExprSyntaxError, neither of which can be
		// statically evaluated here.
		return expr, cty.NilVal, false
	}
}

// evalLogical handles the short-circuiting logical operators. If the result
// is determined by only one of the two operands, the other operand is
// discarded so that its variables do not become dependencies of the
// residual expression.
func (p *partialEvaluator) evalLogical(e *BinaryOpExpr) (Expression, cty.Value, bool, bool) {
	lhsExpr, lhsVal, lhsKnown := p.eval(e.LHS)
	rhsExpr, rhsVal, rhsKnown := p.eval(e.RHS)
	and := e.Op == OpLogicalAnd

	// For "&&" a statically-known false decides the result; for "||" a
	// statically-known true decides it. Conversely, a statically-known
	// true (for "&&") or false (for "||") reduces the expression to just
	// the other operand.
	if lhsKnown {
		if b, ok := staticBool(lhsVal); ok {
			if b != and {
				return &LiteralValueExpr{Val: lhsVal, SrcRange: e.SrcRange}, lhsVal, true, true
			}
			_, marks := lhsVal.Unmark()
			if rhsKnown {
				val := rhsVal.WithMarks(marks)
				return &LiteralValueExpr{Val: val, SrcRange: e.SrcRange}, val, true, true
			}
			return rhsExpr, cty.NilVal, false, true
		}
	}
	if rhsKnown {
		if b, ok := staticBool(rhsVal); ok {
			if b != and {
				return &LiteralValueExpr{Val: rhsVal, SrcRange: e.SrcRange}, rhsVal, true, true
			}
			_, marks := rhsVal.Unmark()
			if lhsKnown {
				val := lhsVal.WithMarks(marks)
				return &LiteralValueExpr{Val: val, SrcRange: e.SrcRange}, val, true, true
			}
			return lhsExpr, cty.NilVal, false, true
		}
	}
	return nil, cty.NilVal, false, false
}

// tryStatic evaluates expr to a literal value if it has no remaining
// variable references and contains no function calls. If evaluation fails,
// the diagnostics are recorded (they carry source ranges) and the
// expression is returned unevaluated.
func (p *partialEvaluator) tryStatic(expr Expression) (Expression, cty.Value, bool) {
	if len(Variables(expr)) != 0 || containsFunctionCall(expr) {
		return expr, cty.NilVal, false
	}
	val, diags := expr.Value(&hcl.EvalContext{})
	p.diags = append(p.diags, diags...)
	if diags.HasErrors() {
		return expr, cty.NilVal, false
	}
	if !val.IsWhollyKnown() {
		return expr, cty.NilVal, false
	}
	return &LiteralValueExpr{Val: val, SrcRange: expr.Range()}, val, true
}

// lookup returns the statically-known value for the given traversal, if any.
func (p *partialEvaluator) lookup(t hcl.Traversal) (cty.Value, bool) {
	if len(t) == 0 || p.known == nil {
		return cty.NilVal, false
	}
	if p.shadowed[t.RootName()] > 0 {
		return cty.NilVal, false
	}
	val, diags := t.TraverseAbs(p.known)
	if diags.HasErrors() || !val.IsWhollyKnown() {
		return cty.NilVal, false
	}
	return val, true
}

// remainingDeps returns the deduplicated traversals that the residual
// expression references but which cannot be resolved to a statically-known
// value in the given context.
func (p *partialEvaluator) remainingDeps(expr Expression) []hcl.Traversal {
	var deps []hcl.Traversal
	seen := make(map[string]struct{})
	for _, t := range Variables(expr) {
		if _, ok := p.lookup(t); ok {
			continue
		}
		key := traversalKey(t)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		deps = append(deps, t)
	}
	return deps
}

func (p *partialEvaluator) shadow(name string) {
	if name == "" {
		return
	}
	if p.shadowed == nil {
		p.shadowed = make(map[string]int)
	}
	p.shadowed[name]++
}

func (p *partialEvaluator) unshadow(name string) {
	if name == "" {
		return
	}
	p.shadowed[name]--
}

// dedupeDiags removes duplicate diagnostics, which can arise when a
// statically-evaluable sub-expression is evaluated once on its own and
// again as part of an enclosing expression.
func (p *partialEvaluator) dedupeDiags() hcl.Diagnostics {
	if len(p.diags) == 0 {
		return nil
	}
	var ret hcl.Diagnostics
	seen := make(map[string]struct{})
	for _, diag := range p.diags {
		key := fmt.Sprintf("%d|%s|%s|%s", diag.Severity, diag.Summary, diag.Detail, rangeKey(diag.Subject))
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		ret = append(ret, diag)
	}
	return ret
}

// staticBool returns the boolean value of a statically-known value if it is
// a non-null bool.
func staticBool(val cty.Value) (bool, bool) {
	uv, _ := val.Unmark()
	if uv.Type() != cty.Bool || uv.IsNull() || !uv.IsKnown() {
		return false, false
	}
	return uv.True(), true
}

// containsFunctionCall returns true if the given expression contains any
// function calls, which partial evaluation must never execute.
func containsFunctionCall(expr Expression) bool {
	found := false
	VisitAll(expr, func(n Node) hcl.Diagnostics {
		if _, ok := n.(*FunctionCallExpr); ok {
			found = true
		}
		return nil
	})
	return found
}

// traversalKey returns a string that uniquely identifies a traversal, for
// deduplication purposes.
func traversalKey(t hcl.Traversal) string {
	key := t.RootName()
	for _, tr := range t[1:] {
		switch tr := tr.(type) {
		case hcl.TraverseAttr:
			key += "." + tr.Name
		case hcl.TraverseIndex:
			key += fmt.Sprintf("[%#v]", tr.Key)
		default:
			key += ".?"
		}
	}
	return key
}

func rangeKey(rng *hcl.Range) string {
	if rng == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d-%d:%d", rng.Filename, rng.Start.Line, rng.Start.Column, rng.End.Line, rng.End.Column)
}
