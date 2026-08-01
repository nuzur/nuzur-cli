package app

import (
	"fmt"
	"strings"

	"github.com/nuzur/nuzur-cli/deploy"
)

// command_destroy_outcome.go turns what a teardown ACTUALLY did into what it says.
//
// The closing summary used to be assembled from what destroy set out to do. Its
// `default:` arm printed "server cleaned up" without consulting whether the teardown
// had succeeded, and the database line was gated on vmDeleted — which is false
// precisely when the VM was already deleted. Destroying a record whose droplet had
// been removed underneath it therefore printed an SSH timeout, "warning: server
// teardown failed", a DigitalOcean 404, and then: "Destroyed deployment X (server
// cleaned up, shared agent revoked — last project on the box)." and "The database was
// kept — pass --purge to drop it." No server was cleaned up, no database was kept:
// it went with the droplet.
//
// So the outcome is recorded as it happens and rendered at the end, rather than
// described in advance.

// serverTeardown is what the on-box cleanup did.
type serverTeardown int

const (
	// teardownSkipped: --skip-server, or a record still marked Provisioning (there is
	// nothing on the box to clean up, and its host may not even be known).
	teardownSkipped serverTeardown = iota
	// teardownDone: the script ran on the box and returned.
	teardownDone
	// teardownFailed: the box did not answer, or the script errored.
	teardownFailed
)

// vmFate is what happened to the managed-provider VM.
type vmFate int

const (
	// vmNotApplicable: BYO-SSH, --keep-vm, or another project still on the box.
	vmNotApplicable vmFate = iota
	// vmDeleted: the provider accepted the delete.
	vmDeleted
	// vmAlreadyGone: the provider says there is no such instance. Nothing is billing
	// and there is nothing to go and remove by hand.
	vmAlreadyGone
	// vmDeleteFailed: a real failure. This is the ONLY case that should send someone
	// to their provider console.
	vmDeleteFailed
	// vmNeverCreated: the create call never took effect, so there was nothing to
	// delete in the first place.
	vmNeverCreated
	// vmKept: --keep-vm.
	vmKept
)

// destroyOutcome is the record of one `nuzur-cli destroy`, filled in as it goes.
type destroyOutcome struct {
	ID       string
	Provider deploy.Provider
	// IsLast is whether this was the last project on the box. When it is not, the
	// box, its agent and its database all survive by design.
	IsLast bool
	// Provisioning marks a record whose deploy died while creating the VM: it never
	// bootstrapped a box, paired an agent or created a database, so the VM is the only
	// thing that was ever real.
	Provisioning bool
	Server       serverTeardown
	VM           vmFate
	// Revoked is whether the shared agent was actually revoked in nuzur (not merely
	// whether a revoke was appropriate).
	Revoked bool
	// Purge is --purge: the teardown script was asked to DROP the database.
	Purge bool
	// ExternalDB marks a --db-dsn/--connection database, which lives somewhere else
	// entirely and survives everything here.
	ExternalDB bool
}

// summary is the closing report: a headline naming what happened to the box, plus
// the database's fate on its own line when it is known.
func (o destroyOutcome) summary() []string {
	lines := []string{o.headline()}
	if db := o.databaseLine(); db != "" {
		lines = append(lines, "  "+db)
	}
	return lines
}

// headline says what was removed, claiming only what actually happened.
func (o destroyOutcome) headline() string {
	switch {
	case o.Provisioning:
		// An interrupted provision never paired an agent or bootstrapped the box, so
		// claiming either would be a lie. Only the VM was ever real.
		if o.VM == vmDeleted {
			return fmt.Sprintf("Destroyed deployment %s (this deploy was interrupted while creating the server; the VM was deleted).", o.ID)
		}
		if o.VM == vmAlreadyGone {
			return fmt.Sprintf("Destroyed deployment %s (this deploy was interrupted while creating the server; its %s VM no longer exists — nothing is billing).", o.ID, o.Provider)
		}
		return fmt.Sprintf("Destroyed deployment %s (this deploy was interrupted before the server was created; nothing to clean up).", o.ID)
	case !o.IsLast:
		return fmt.Sprintf("Destroyed deployment %s (this project removed; the box's shared agent stays for its other projects).", o.ID)
	}

	var clauses []string
	switch o.Server {
	case teardownDone:
		clauses = append(clauses, "server cleaned up")
	case teardownFailed:
		clauses = append(clauses, "the server was unreachable, so its on-box cleanup was SKIPPED")
	case teardownSkipped:
		clauses = append(clauses, "the server was left untouched")
	}
	switch o.VM {
	case vmDeleted:
		clauses = append(clauses, "VM deleted")
	case vmAlreadyGone:
		clauses = append(clauses, fmt.Sprintf("its %s VM no longer exists — nothing is billing", o.Provider))
	case vmDeleteFailed:
		clauses = append(clauses, fmt.Sprintf("the %s VM could NOT be deleted (see the warning above)", o.Provider))
	case vmKept:
		clauses = append(clauses, "the VM was kept (--keep-vm)")
	case vmNeverCreated:
		clauses = append(clauses, "no VM was ever created")
	}
	// Said explicitly whenever the box was not cleaned up, because then this is the
	// only thing that DID happen and the user needs to know the record is gone.
	if o.Server != teardownDone {
		clauses = append(clauses, "local and cloud records removed")
	}
	if o.Revoked {
		clauses = append(clauses, "shared agent revoked")
	}
	return fmt.Sprintf("Destroyed deployment %s (%s — last project on the box).", o.ID, strings.Join(clauses, "; "))
}

// databaseLine reports the database's fate, and ONLY when it is known. Silence is
// the right answer for a database whose state nothing here established.
func (o destroyOutcome) databaseLine() string {
	switch {
	case o.Provisioning:
		// No box was ever bootstrapped, so no database was ever created.
		return ""
	case o.ExternalDB:
		// --db-dsn/--connection: the user's own database, somewhere else. It survives
		// everything above, including a deleted VM.
		return "The database is external (--db-dsn/--connection) and is untouched — destroying this deployment does not affect it."
	case o.VM == vmDeleted:
		return "The database was on that VM and went with it."
	case o.VM == vmAlreadyGone:
		// The fate that was being reported as "the database was kept".
		return "The database was on that VM, which no longer exists, so it is gone too."
	case !o.IsLast:
		// The box and its database keep serving the other projects on it.
		return "This project's database was removed from the box; the box and its other databases are untouched."
	case o.Server == teardownDone && o.Purge:
		return "The database and its app user were dropped (--purge)."
	case o.Server == teardownDone:
		return "The database was kept — pass --purge to drop it."
	case o.Server == teardownFailed && o.Purge:
		return "The database was NOT dropped: --purge runs on the box, and the box did not answer."
	case o.Server == teardownFailed:
		return "The database was neither kept nor dropped on purpose — nothing ran on the box. Whatever is on it is still there if it comes back."
	case o.Server == teardownSkipped:
		return "The database was not touched — nothing was run on the box (--skip-server)."
	}
	return ""
}
