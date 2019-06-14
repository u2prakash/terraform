package terraform

import (
	"fmt"
	"log"

	"github.com/hashicorp/hcl2/hcl"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/plans"
	"github.com/hashicorp/terraform/plans/objchange"
	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform/tfdiags"
)

// EvalPlanOutput is an EvalNode implementation that creates a planned change
// for a specific output value and records it in the planned changeset.
type EvalPlanOutput struct {
	Addr         addrs.OutputValue
	Config       *configs.Output
	ForceDestroy bool
}

// Eval implements EvalNode
func (n *EvalPlanOutput) Eval(ctx EvalContext) (interface{}, error) {
	var diags tfdiags.Diagnostics
	addr := n.Addr.Absolute(ctx.Path())

	changes := ctx.Changes()
	state := ctx.State()
	var os *states.OutputValue
	if state != nil {
		os = state.OutputValue(addr)
	}

	before := cty.NullVal(cty.DynamicPseudoType)
	if os != nil {
		before = os.Value
	}
	sensitive := false
	if os != nil {
		before = os.Value
		if os.Sensitive {
			sensitive = true
		}
	}
	if n.Config != nil {
		if n.Config.Sensitive {
			sensitive = true
		}
	}

	var change *plans.OutputChange
	switch {
	case n.Config == nil || n.ForceDestroy:
		change = &plans.OutputChange{
			Addr: addr,
			Change: plans.Change{
				Action: plans.Delete,
				Before: before,
				After:  cty.NullVal(cty.DynamicPseudoType),
			},
			Sensitive: sensitive,
		}
	default:
		after, moreDiags := ctx.EvaluateExpr(n.Config.Expr, cty.DynamicPseudoType, nil)
		diags = diags.Append(moreDiags)
		if moreDiags.HasErrors() {
			return nil, diags.Err()
		}

		eqV := after.Equals(before)
		eq := eqV.IsKnown() && eqV.True()
		var action plans.Action
		switch {
		case eq:
			action = plans.NoOp
		case os == nil:
			action = plans.Create
		default:
			action = plans.Update
		}

		change = &plans.OutputChange{
			Addr: addr,
			Change: plans.Change{
				Action: action,
				Before: before,
				After:  after,
			},
			Sensitive: sensitive,
		}
	}

	changeSrc, err := change.Encode()
	if err != nil {
		return nil, fmt.Errorf("failed to encode plan for %s: %s", addr, err)
	}
	log.Printf("[TRACE] EvalPlanOutput: Recording %s change for %s", changeSrc.Action, addr)
	changes.AppendOutputChange(changeSrc)

	// We'll also record the planned value in the state for consistency,
	// but expression evaluation during the plan walk should always prefer
	// to use the value from the changeset because the state can't represent
	// unknown values.
	state.SetOutputValue(addr, cty.UnknownAsNull(change.After), change.Sensitive)

	return nil, diags.ErrWithWarnings()
}

// EvalApplyOutput is an EvalNode implementation that handles a
// previously-planned change to an output value.
type EvalApplyOutput struct {
	Addr addrs.OutputValue
	Expr hcl.Expression
}

// Eval implements EvalNode
func (n *EvalApplyOutput) Eval(ctx EvalContext) (interface{}, error) {
	var diags tfdiags.Diagnostics
	addr := n.Addr.Absolute(ctx.Path())

	state := ctx.State()
	changes := ctx.Changes()
	if changes == nil {
		// This is unexpected, but we'll tolerate it so that we can run
		// context tests with incomplete mocks.
		log.Printf("[WARN] EvalApplyOutput for %s with no active changeset is no-op", addr)
		return nil, nil
	}

	changeSrc := changes.GetOutputChange(addr)
	if changeSrc == nil || changeSrc.Action == plans.NoOp {
		log.Printf("[WARN] EvalApplyOutput: %s has no change planned", addr)
		return nil, nil
	}

	change, err := changeSrc.Decode()
	if err != nil {
		// This shouldn't happen unless someone tampered with the plan file
		// or there is a bug in the plan file reader/writer.
		return nil, fmt.Errorf("failed to decode plan for %s: %s", addr, err)
	}

	log.Printf("[TRACE] EvalApplyOutput: applying %s change for %s", change.Action, addr)

	switch change.Action {
	case plans.Delete:
		state.RemoveOutputValue(addr)
	default:
		// The "after" value in our planned change might be incomplete if
		// it was constructed from unknown values during planning, so we
		// need to re-evaluate it here to incorporate new values we've
		// learned so far during the apply walk.
		val, moreDiags := ctx.EvaluateExpr(n.Expr, cty.DynamicPseudoType, nil)
		diags = diags.Append(moreDiags)
		if moreDiags.HasErrors() {
			return nil, diags.Err()
		}

		if !val.IsWhollyKnown() {
			// If anything is left unknown during apply, that suggests a bug
			// in some other part of Terraform, such as a missing edge in
			// the dependency graph. We don't have enough information here to
			// diagnose the reason, but we'll report it because we can't
			// save an unknown value in the state.
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid output value result",
				Detail: fmt.Sprintf(
					"The result of %s contains unknown values. This is a bug in Terraform Core; please report it!",
					addr,
				),
				Subject: n.Expr.Range().Ptr(),
			})
			return nil, diags.Err()
		}

		if errs := objchange.AssertValueCompatible(change.After, val); len(errs) > 0 {
			// This should not happen, but one way it could happen is if
			// a resource in the configuration is written with the legacy
			// SDK and is thus exempted from the usual provider result safety
			// checks that would otherwise have caught this upstream.
			if change.Sensitive {
				// A more general message to avoid disclosing any details about
				// the sensitive value.
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Output has inconsistent result during apply",
					Detail: fmt.Sprintf(
						"When updating %s to include new values learned so far during apply, the value changed unexpectedly.\n\nThis usually indicates a bug in a provider whose results are used in this output's value expression.",
						addr,
					),
					Subject: n.Expr.Range().Ptr(),
				})
			} else {
				for _, err := range errs {
					diags = diags.Append(&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Output has inconsistent result during apply",
						Detail: fmt.Sprintf(
							"When updating %s to include new values learned so far during apply, the value changed unexpectedly: %s.\n\nThis usually indicates a bug in a provider whose results are used in this output's value expression.",
							addr, tfdiags.FormatError(err),
						),
						Subject: n.Expr.Range().Ptr(),
					})
				}
			}

			// NOTE: We do still proceed to save the updated value below,
			// in case a subsequent codepath inspects it. This is consistent
			// with how we handle inconsistent results from apply for
			// resources.
		}

		// If we had an unknown value during planning then we would've planned
		// an update, but that unknown can turn out to be null, so we'll
		// handle that as a special case here.
		if val.IsNull() {
			log.Printf("[TRACE] EvalApplyOutput: Removing %s from state (it is now null)", addr)
			state.RemoveOutputValue(addr)
		} else {
			log.Printf("[TRACE] EvalApplyOutput: Saving new value for %s in state", addr)
			state.SetOutputValue(addr, val, change.Sensitive)
		}
	}

	return nil, diags.ErrWithWarnings()
}

// EvalRefreshOutput is an EvalNode implementation that re-evaluates a given
// output value and updates its cached value in the state.
//
// This EvalNode is only for walks where no direct (user-initiated) changes to
// output values are expected, such as the refresh walk. The plan and apply
// walks must instead use EvalPlanOutput and EvalApplyOutput respectively.
type EvalRefreshOutput struct {
	Addr      addrs.OutputValue
	Sensitive bool
	Expr      hcl.Expression
}

// Eval implements EvalNode
func (n *EvalRefreshOutput) Eval(ctx EvalContext) (interface{}, error) {
	addr := n.Addr.Absolute(ctx.Path())

	// This has to run before we have a state lock, since evaluation also
	// reads the state
	val, diags := ctx.EvaluateExpr(n.Expr, cty.DynamicPseudoType, nil)
	// We'll handle errors below, after we have loaded the module.

	state := ctx.State()
	if state == nil {
		return nil, nil
	}

	// handling the interpolation error
	if diags.HasErrors() {
		return nil, diags.Err()
	}

	if !val.IsWhollyKnown() {
		// Output values should produce unknown values only during the plan
		// walk, which we deal with in EvalPlanOutput instead.
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid output value result",
			Detail: fmt.Sprintf(
				"The result of %s contains unknown values. This is a bug in Terraform Core; please report it!",
				addr,
			),
			Subject: n.Expr.Range().Ptr(),
		})
		return nil, diags.Err()
	}

	if val.IsNull() {
		log.Printf("[TRACE] EvalRefreshOutput: Removing %s from state (it is now null)", addr)
		state.RemoveOutputValue(addr)
	} else {
		log.Printf("[TRACE] EvalRefreshOutput: Saving value for %s in state", addr)
		state.SetOutputValue(addr, val, n.Sensitive)
	}

	return nil, diags.ErrWithWarnings()
}
