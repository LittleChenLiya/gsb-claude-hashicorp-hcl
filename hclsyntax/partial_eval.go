// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package hclsyntax

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// PartialEval performs static partial evaluation of the given expression
// using the variables available in the given evaluation context as the set
// of "known" values.
//
// Any sub-expression that can be resolved using only literal values and
// known variables is folded into a literal value. Conditional expressions
// whose condition is decidable are reduced to only their reachable branch,
// so references in the unreachable branch are not reported as remaining
// dependencies. Conditional expressions with an undecidable condition
// retain both branches.
//
// Function calls are never executed during partial evaluation, even if the
// called function is registered in the given evaluation context. A call
// whose arguments can be partially evaluated is returned with its arguments
// reduced, but the call itself is always left in place for the final
// evaluation.
//
// The result is the residual expression along with the traversals of the
// variables it still depends on. Evaluating the residual expression with an
// evaluation context that additionally provides values for those remaining
// variables produces the same result as evaluating the original expression
// with the union of the known and remaining variables.
//
// The returned diagnostics describe genuine errors that can be detected
// statically, such as indexing out of the bounds of a known tuple, each
// carrying the source range of the offending sub-expression. References to
// unknown variables are not errors: they are reported via the returned
// traversals instead.
func PartialEval(expr Expression, ctx *hcl.EvalContext) (Expression, []hcl.Traversal, hcl.Diagnostics) {
	if ctx == nil {
		ctx = &hcl.EvalContext{}
	}
	p := &partialEvaluator{
		ctx:  ctx,
		seen: make(map[string]struct{}),
	}
	residual := p.eval(expr)

	// The remaining dependencies are the root-scope traversals that were
	// not resolvable from the given context. Variables of the residual
	// expression whose root is already known are not reported, since the
	// caller has already supplied them.
	var deps []hcl.Traversal
	for _, traversal := range Variables(residual) {
		if _, known := ctx.Variables[traversal.RootName()]; known {
			continue
		}
		deps = append(deps, traversal)
	}
	return residual, dedupeTraversals(deps), p.diags
}

// partialEvaluator holds the state shared across the recursive partial
// evaluation of a single expression.
type partialEvaluator struct {
	ctx   *hcl.EvalContext
	diags hcl.Diagnostics

	// locals is a stack of names that are bound by enclosing for
	// expressions and therefore shadow any known variables of the same
	// name.
	locals []string

	// seen deduplicates genuine diagnostics, because a sub-expression that
	// fails to evaluate may be re-evaluated speculatively by its ancestors.
	seen map[string]struct{}
}

// eval returns the residual form of the given expression, folding whatever
// can be decided statically.
func (p *partialEvaluator) eval(expr Expression) Expression {
	switch e := expr.(type) {
	case *LiteralValueExpr:
		return e
	case *AnonSymbolExpr:
		// The anonymous symbol of a splat expression is meaningful only
		// to its parent SplatExpr, so we leave it alone.
		return e
	case *ParenthesesExpr:
		// Parentheses have no semantic effect once the expression tree
		// has been parsed, so we can drop them.
		return p.eval(e.Expression)
	case *ScopeTraversalExpr:
		if p.isLocal(e.Traversal.RootName()) {
			return e
		}
		return p.foldOrKeep(e)
	case *RelativeTraversalExpr:
		return p.foldOrKeep(&RelativeTraversalExpr{
			Source:    p.eval(e.Source),
			Traversal: e.Traversal,
			SrcRange:  e.SrcRange,
		})
	case *IndexExpr:
		return p.foldOrKeep(&IndexExpr{
			Collection:   p.eval(e.Collection),
			Key:          p.eval(e.Key),
			SrcRange:     e.SrcRange,
			OpenRange:    e.OpenRange,
			BracketRange: e.BracketRange,
		})
	case *UnaryOpExpr:
		return p.foldOrKeep(&UnaryOpExpr{
			Op:          e.Op,
			Val:         p.eval(e.Val),
			SrcRange:    e.SrcRange,
			SymbolRange: e.SymbolRange,
		})
	case *BinaryOpExpr:
		return p.foldOrKeep(&BinaryOpExpr{
			LHS:      p.eval(e.LHS),
			Op:       e.Op,
			RHS:      p.eval(e.RHS),
			SrcRange: e.SrcRange,
		})
	case *ConditionalExpr:
		return p.evalConditional(e)
	case *FunctionCallExpr:
		// Function calls are never executed during partial evaluation,
		// even if the function is registered in the evaluation context.
		// We still reduce the arguments so that any pure-literal
		// computation inside them is folded.
		args := make([]Expression, len(e.Args))
		for i, arg := range e.Args {
			args[i] = p.eval(arg)
		}
		return &FunctionCallExpr{
			Name:            e.Name,
			Args:            args,
			ExpandFinal:     e.ExpandFinal,
			NameRange:       e.NameRange,
			OpenParenRange:  e.OpenParenRange,
			CloseParenRange: e.CloseParenRange,
		}
	case *TupleConsExpr:
		exprs := make([]Expression, len(e.Exprs))
		for i, elem := range e.Exprs {
			exprs[i] = p.eval(elem)
		}
		return p.foldOrKeep(&TupleConsExpr{
			Exprs:     exprs,
			SrcRange:  e.SrcRange,
			OpenRange: e.OpenRange,
		})
	case *ObjectConsExpr:
		items := make([]ObjectConsItem, len(e.Items))
		for i, item := range e.Items {
			items[i] = ObjectConsItem{
				KeyExpr:   p.eval(item.KeyExpr),
				ValueExpr: p.eval(item.ValueExpr),
			}
		}
		return p.foldOrKeep(&ObjectConsExpr{
			Items:     items,
			SrcRange:  e.SrcRange,
			OpenRange: e.OpenRange,
		})
	case *ObjectConsKeyExpr:
		// A naked identifier key is a literal string, not a variable
		// reference, so we must not try to resolve it against the known
		// variables.
		if !e.ForceNonLiteral && e.literalName() != "" {
			return e
		}
		return &ObjectConsKeyExpr{
			Wrapped:         p.eval(e.Wrapped),
			ForceNonLiteral: e.ForceNonLiteral,
		}
	case *TemplateExpr:
		parts := make([]Expression, len(e.Parts))
		for i, part := range e.Parts {
			parts[i] = p.eval(part)
		}
		return p.foldOrKeep(&TemplateExpr{
			Parts:    parts,
			SrcRange: e.SrcRange,
		})
	case *TemplateWrapExpr:
		return p.foldOrKeep(&TemplateWrapExpr{
			Wrapped:  p.eval(e.Wrapped),
			SrcRange: e.SrcRange,
		})
	case *TemplateJoinExpr:
		return p.foldOrKeep(&TemplateJoinExpr{
			Tuple: p.eval(e.Tuple),
		})
	case *ForExpr:
		// The key/value/condition expressions are evaluated in a child
		// scope that binds the iteration variables, so we reduce them
		// with those names masked out to prevent them from being
		// resolved against the known variables. If the collection and
		// everything the body closes over are known then the whole for
		// expression folds into a literal.
		coll := p.eval(e.CollExpr)
		restore := p.pushLocals(e.KeyVar, e.ValVar)
		var keyExpr, valExpr, condExpr Expression
		if e.KeyExpr != nil {
			keyExpr = p.eval(e.KeyExpr)
		}
		if e.ValExpr != nil {
			valExpr = p.eval(e.ValExpr)
		}
		if e.CondExpr != nil {
			condExpr = p.eval(e.CondExpr)
		}
		restore()
		return p.foldOrKeep(&ForExpr{
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
		return p.foldOrKeep(&SplatExpr{
			Source:      p.eval(e.Source),
			Each:        p.eval(e.Each),
			Item:        e.Item,
			SrcRange:    e.SrcRange,
			MarkerRange: e.MarkerRange,
		})
	default:
		return expr
	}
}

// pushLocals masks the given names as for-expression locals. The returned
// function restores the previous set of local names.
func (p *partialEvaluator) pushLocals(names ...string) (restore func()) {
	saved := p.locals
	for _, name := range names {
		if name == "" {
			continue
		}
		p.locals = append(p.locals, name)
	}
	return func() {
		p.locals = saved
	}
}

// isLocal returns true if the given name is currently bound as a
// for-expression local.
func (p *partialEvaluator) isLocal(name string) bool {
	for _, local := range p.locals {
		if local == name {
			return true
		}
	}
	return false
}

// evalConditional partially evaluates a conditional expression, reducing it
// to only its reachable branch when the condition is decidable.
func (p *partialEvaluator) evalConditional(e *ConditionalExpr) Expression {
	cond := p.eval(e.Condition)

	if lit, ok := cond.(*LiteralValueExpr); ok {
		unmarked, marks := lit.Val.Unmark()
		condVal, convErr := convert.Convert(unmarked, cty.Bool)
		switch {
		case unmarked.IsNull():
			p.emitDiag(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Null condition",
				Detail:   "The condition value is null. Conditions must either be true or false.",
				Subject:  e.Condition.Range().Ptr(),
				Context:  &e.SrcRange,
			})
		case convErr != nil:
			p.emitDiag(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Incorrect condition type",
				Detail:   "The condition expression must be of type bool.",
				Subject:  e.Condition.Range().Ptr(),
				Context:  &e.SrcRange,
			})
		default:
			// The condition is decidable, so only the reachable branch
			// survives and references in the other branch do not become
			// remaining dependencies.
			branch := e.FalseResult
			if condVal.True() {
				branch = e.TrueResult
			}
			residual := p.eval(branch)
			if len(marks) == 0 {
				return residual
			}
			// A marked (for example, sensitive) condition contributes its
			// marks to the result. We can propagate them statically only
			// if the selected branch folded to a literal.
			if litRes, ok := residual.(*LiteralValueExpr); ok {
				return &LiteralValueExpr{
					Val:      litRes.Val.WithMarks(marks),
					SrcRange: litRes.SrcRange,
				}
			}
		}
	}

	// The condition is not decidable (or is erroneous), so we retain both
	// branches.
	return &ConditionalExpr{
		Condition:   cond,
		TrueResult:  p.eval(e.TrueResult),
		FalseResult: p.eval(e.FalseResult),
		SrcRange:    e.SrcRange,
	}
}

// foldOrKeep attempts to evaluate the given (already partially reduced)
// expression to a literal value. If the expression cannot be fully resolved
// with the known variables, it is returned unchanged as the residual.
func (p *partialEvaluator) foldOrKeep(expr Expression) Expression {
	val, ok := p.tryEval(expr)
	if !ok {
		return expr
	}
	return &LiteralValueExpr{
		Val:      val,
		SrcRange: expr.Range(),
	}
}

// tryEval evaluates expr with the known variables, succeeding only if the
// expression contains no function calls and evaluates to a wholly-known
// value without errors.
func (p *partialEvaluator) tryEval(expr Expression) (cty.Value, bool) {
	// Function calls must never be executed during partial evaluation,
	// even if the function is registered in the evaluation context.
	if containsFunctionCall(expr) {
		return cty.NilVal, false
	}

	val, diags := expr.Value(p.evalCtx())
	if diags.HasErrors() {
		// If every variable the expression refers to is known then these
		// diagnostics describe genuine errors (such as an out-of-bounds
		// index into a known tuple) rather than missing information, so
		// we report them to the caller.
		if p.allRootsKnown(expr) {
			p.emitDiags(diags)
		}
		return cty.NilVal, false
	}
	if !val.IsWhollyKnown() {
		return cty.NilVal, false
	}
	return val, true
}

// evalCtx returns the evaluation context to use for speculative evaluation.
// Any for-expression locals currently in scope are masked with unknown
// values so that they cannot be resolved against the known variables, even
// if a known variable happens to share the same name.
func (p *partialEvaluator) evalCtx() *hcl.EvalContext {
	if len(p.locals) == 0 {
		return p.ctx
	}
	child := p.ctx.NewChild()
	child.Variables = make(map[string]cty.Value, len(p.locals))
	for _, name := range p.locals {
		child.Variables[name] = cty.DynamicVal
	}
	return child
}

// allRootsKnown returns true if every root-scope variable referenced by the
// given expression has a value in the evaluation context.
func (p *partialEvaluator) allRootsKnown(expr Expression) bool {
	for _, traversal := range Variables(expr) {
		if p.isLocal(traversal.RootName()) {
			return false
		}
		if _, ok := p.ctx.Variables[traversal.RootName()]; !ok {
			return false
		}
	}
	return true
}

// emitDiag appends a single diagnostic, suppressing duplicates.
func (p *partialEvaluator) emitDiag(diag *hcl.Diagnostic) {
	p.emitDiags(hcl.Diagnostics{diag})
}

// emitDiags appends the given diagnostics, suppressing duplicates that can
// arise when a failing sub-expression is speculatively re-evaluated by its
// ancestors.
func (p *partialEvaluator) emitDiags(diags hcl.Diagnostics) {
	for _, diag := range diags {
		key := diagKey(diag)
		if _, dup := p.seen[key]; dup {
			continue
		}
		p.seen[key] = struct{}{}
		p.diags = append(p.diags, diag)
	}
}

func diagKey(diag *hcl.Diagnostic) string {
	subject := ""
	if diag.Subject != nil {
		subject = diag.Subject.String()
	}
	context := ""
	if diag.Context != nil {
		context = diag.Context.String()
	}
	return fmt.Sprintf("%d|%s|%s|%s|%s", diag.Severity, diag.Summary, diag.Detail, subject, context)
}

// containsFunctionCall returns true if the given expression contains any
// function call nodes.
func containsFunctionCall(expr Expression) bool {
	found := false
	//nolint:errcheck // the visit function never returns diagnostics
	VisitAll(expr, func(n Node) hcl.Diagnostics {
		if _, ok := n.(*FunctionCallExpr); ok {
			found = true
		}
		return nil
	})
	return found
}

// dedupeTraversals removes duplicate traversals from the given slice while
// preserving their order.
func dedupeTraversals(traversals []hcl.Traversal) []hcl.Traversal {
	if len(traversals) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(traversals))
	var ret []hcl.Traversal
	for _, traversal := range traversals {
		key := traversalKey(traversal)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		ret = append(ret, traversal)
	}
	return ret
}

func traversalKey(traversal hcl.Traversal) string {
	var sb strings.Builder
	for _, traverser := range traversal {
		switch t := traverser.(type) {
		case hcl.TraverseRoot:
			fmt.Fprintf(&sb, "root:%s;", t.Name)
		case hcl.TraverseAttr:
			fmt.Fprintf(&sb, "attr:%s;", t.Name)
		case hcl.TraverseIndex:
			fmt.Fprintf(&sb, "index:%#v;", t.Key)
		case hcl.TraverseSplat:
			sb.WriteString("splat;")
		default:
			fmt.Fprintf(&sb, "%T;", traverser)
		}
	}
	return sb.String()
}
