package app

import (
	"fmt"
	"os"

	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/sqlplan"
	"github.com/urfave/cli"
)

// deploy_schema_gate.go decides whether a deploy is allowed to apply the migration
// sql-push has computed.
//
// Deploy is declarative: whatever exists in the database and not in the model is,
// by definition, surplus, and reconciling means dropping it. That is the tool's
// whole value when the model is the source of truth, and its whole danger when the
// model has fallen behind — which is the normal state of a database somebody has
// been hand-patching. Until this gate existed, those two cases were indistinguishable
// from the outside: the deploy applied whatever the differ produced and reported
// success either way.

// schemaGateResult is what the gate decided, reported back out of the decider — the
// StepDecider signature has nowhere to return it, and the deploy's outcome summary
// and its revision message in nuzur both need to know.
type schemaGateResult struct {
	plan sqlplan.Plan
	// blocked marks a migration that was rejected for want of --allow-destructive.
	blocked bool
	// destructiveApplied marks a migration that deleted data WITH authorization, so
	// the deployment history can say so.
	destructiveApplied bool
}

// decideSchemaApply is the gate.
//
//	no data-loss statements         → apply, exactly as deploy always has
//	data loss + --allow-destructive → apply, having printed what is being dropped
//	data loss, no flag              → reject; the extension cancels and runs nothing
//
// Only data loss blocks. Index and constraint drops, and type or nullability
// changes, are printed and allowed: gating on those would block ordinary schema
// evolution, and a gate that fires on ordinary work teaches people to pass
// --allow-destructive reflexively, which costs the flag all its meaning.
//
// This is also why MySQL's reconstructed-side churn does not need special handling
// here. That churn is entirely column redefinitions; a phantom ALTER cannot
// manufacture a DROP TABLE or a DROP COLUMN, because those only arise from an object
// genuinely absent on the model side. Gating on data loss alone is correct
// independently, and MySQL is a second argument for it rather than an exception.
func decideSchemaApply(p sqlplan.Plan, allowDestructive bool) (bool, string) {
	dest := p.Destructive()
	switch {
	case len(dest) == 0:
		return true, "no statements delete data"
	case allowDestructive:
		return true, fmt.Sprintf("--allow-destructive authorized %d destructive statement(s)", len(dest))
	default:
		return false, fmt.Sprintf("%d statement(s) would delete data and --allow-destructive was not passed", len(dest))
	}
}

// schemaApplyDecider answers sql-push's confirmation step for a real deploy, having
// first read the migration it is being asked to approve.
func (i *Implementation) schemaApplyDecider(allowDestructive bool, out *schemaGateResult) extensionrun.StepDecider {
	return func(prompt extensionrun.StepPrompt) (extensionrun.StepDecision, error) {
		plan := sqlplan.Analyze(prompt.Content)
		out.plan = plan

		confirm, reason := decideSchemaApply(plan, allowDestructive)
		if !confirm {
			out.blocked = true
			// Printed here rather than by the caller because this is the moment the
			// decision is made, and the plan is in hand.
			outputtools.PrintlnColoredErr("\nSchema NOT applied — this migration deletes data.", outputtools.Red)
			outputtools.PrintlnColoredErr(plan.RenderDestructive(), outputtools.Red)
			return extensionrun.StepDecision{Confirm: false, Reason: reason}, nil
		}
		if plan.HasDestructive() {
			out.destructiveApplied = true
			outputtools.PrintlnColoredErr("\nApplying a migration that DELETES DATA (--allow-destructive was passed):", outputtools.Yellow)
			outputtools.PrintlnColoredErr(plan.RenderDestructive(), outputtools.Yellow)
		}
		return extensionrun.StepDecision{Confirm: true, Reason: reason}, nil
	}
}

// errSchemaBlocked distinguishes "the deploy refused to apply this" from "applying
// it failed", which the outcome reporting has to tell apart: one is a decision
// waiting on a human, the other is a fault to retry.
var errSchemaBlocked = fmt.Errorf("schema apply blocked: the plan deletes data and --allow-destructive was not passed")

// preflightSchemaGate runs the destructive gate BEFORE the deploy ships anything.
//
// The apply itself stays where it has always been — step 10, after the box is
// bootstrapped and the agent is online — because that is the only point at which the
// database is guaranteed to exist. But on a RE-deploy the box, its agent and its
// database already exist, so the migration is computable up front, and the gate's
// decision does not have to wait for the apply.
//
// It has to be made up front, because refusing at step 10 is refusing too late: the
// image has been rebuilt and the container restarted by then, so a gate that blocks
// leaves new code serving an old schema. The safety feature caused the outage it
// exists to prevent. Checking here means a blocked deploy ships nothing at all and
// the box keeps serving the code that matches its database.
//
// Deliberately best-effort in one direction only: a plan that WAS computed and IS
// destructive-without-authorization stops the deploy; anything else (the box is down,
// the agent is offline, the diff errored) proceeds, because a deploy that would
// repair an unreachable box must not be blocked by its being unreachable — and the
// step-10 gate still stands behind it.
//
// First deploys are not covered and do not need to be: the agent does not exist until
// the bootstrap pairs it, and an empty database holds nothing to drop.
func (i *Implementation) preflightSchemaGate(targets *runTargets, t planTarget) error {
	applySQL, _, err := i.computeSchemaPlan(targets, t)
	if err != nil {
		outputtools.PrintlnColoredErr(
			"Could not check the schema migration before shipping (continuing; it is checked again "+
				"before it is applied): "+err.Error(), outputtools.Yellow)
		return nil
	}
	plan := sqlplan.Analyze(applySQL)
	if confirm, _ := decideSchemaApply(plan, false); confirm {
		return nil
	}

	dest := plan.Destructive()
	outputtools.PrintlnColoredErr("\nSchema NOT applied — this migration deletes data.", outputtools.Red)
	outputtools.PrintlnColoredErr(plan.RenderDestructive(), outputtools.Red)
	outputtools.PrintlnColoredErr(fmt.Sprintf(
		"\nThe deploy stopped here, before anything was shipped: %s would DELETE DATA and "+
			"--allow-destructive was not passed. The database is untouched and the app on this box is "+
			"untouched — it is still running the code that matches its schema.",
		plural(len(dest), "statement")), outputtools.Red)
	fmt.Fprintln(os.Stderr)
	outputtools.PrintlnColoredErr("To see the full plan without applying anything:", outputtools.Yellow)
	outputtools.PrintlnColoredErr("  "+rerunCommand(os.Args, false)+" --plan", outputtools.Yellow)
	outputtools.PrintlnColoredErr("To apply it, including the destructive statements:", outputtools.Yellow)
	outputtools.PrintlnColoredErr("  "+rerunCommand(os.Args, true), outputtools.Yellow)
	// A bare exit error: everything worth saying has been printed, and the deployment
	// revision must not be relabelled FAILED — no deploy was attempted.
	return cli.NewExitError("", 1)
}

// revisionShouldFail reports whether a deploy error means the deployment revision in
// nuzur should be marked FAILED.
//
// A blocked destructive schema returns a bare exit error so CI notices, but the box
// genuinely is provisioned and serving, and the shortfall is already recorded in the
// revision's message. Relabelling it a failed deploy would be wrong in the other
// direction — it would say the deploy did not happen.
func revisionShouldFail(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*cli.ExitError); ok {
		return false
	}
	return true
}

// exitCodeForOutcome turns a finished deploy into a process exit code.
//
// A schema that did not reach the database exits non-zero, whether the gate refused
// it or applying it errored. The app is up and serving, but it is serving against a
// database its generated code does not match, and a green CI light on that state is
// precisely the failure this whole feature exists to prevent.
//
// The failed-apply case used to exit 0, which made it the quieter of the two — and it
// is the more dangerous one: a block is a decision nuzur made having sent nothing,
// whereas a failure means a statement reached the database and errored, which is also
// the only path on which a migration can land half-applied.
func exitCodeForOutcome(o deployOutcome) error {
	if o.schemaBlocked || !o.schemaApplied {
		return cli.NewExitError("", 1)
	}
	return nil
}

// printGateFollowUp tells a user whose deploy was blocked what to do next. Written
// to stderr after the deployment report, where the rest of the closing summary goes.
func printGateFollowUp(o deployOutcome) {
	if !o.schemaBlocked {
		return
	}
	fmt.Fprintln(os.Stderr)
	outputtools.PrintlnColoredErr("To see the full plan without applying anything:", outputtools.Yellow)
	outputtools.PrintlnColoredErr("  "+rerunCommand(os.Args, false)+" --plan", outputtools.Yellow)
	if o.rerunCommand != "" {
		outputtools.PrintlnColoredErr("To apply it, including the destructive statements:", outputtools.Yellow)
		outputtools.PrintlnColoredErr("  "+o.rerunCommand, outputtools.Yellow)
	}
}
