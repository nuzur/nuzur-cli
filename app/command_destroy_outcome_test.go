package app

import (
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

// The reported case, verbatim: the droplet was deleted out from under the record, so
// `nuzur-cli destroy r6box-c738c60c` printed an SSH timeout, "warning: server teardown
// failed", a DigitalOcean 404 — and then claimed three things that did not happen:
// "server cleaned up", "shared agent revoked — last project on the box" and "The
// database was kept — pass --purge to drop it". No server was cleaned up, and the
// database went with the droplet.
func TestDestroyOutcomeOnADeadBox(t *testing.T) {
	o := destroyOutcome{
		ID:       "r6box-c738c60c",
		Provider: deploy.ProviderDigitalOcean,
		IsLast:   true,
		Server:   teardownFailed,
		VM:       vmAlreadyGone,
		Revoked:  true,
	}
	got := strings.Join(o.summary(), "\n")

	for _, want := range []string{
		"Destroyed deployment r6box-c738c60c",
		"the server was unreachable, so its on-box cleanup was SKIPPED",
		"its digitalocean VM no longer exists — nothing is billing",
		"local and cloud records removed",
		"shared agent revoked",
		"The database was on that VM, which no longer exists, so it is gone too.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q\ngot:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"server cleaned up", "The database was kept", "--purge to drop it"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("summary claims %q, which did not happen\ngot:\n%s", forbidden, got)
		}
	}
}

func TestDestroyOutcomeSummary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		o         destroyOutcome
		wantIn    []string
		wantNotIn []string
	}{
		{
			name: "a clean teardown of the last project says what it did",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderDigitalOcean, IsLast: true,
				Server: teardownDone, VM: vmDeleted, Revoked: true,
			},
			wantIn: []string{
				"server cleaned up", "VM deleted", "shared agent revoked",
				"last project on the box",
				// The VM took the database with it, so "kept — pass --purge" would be
				// nonsense: there is nothing left to purge.
				"The database was on that VM and went with it.",
			},
			wantNotIn: []string{"was kept"},
		},
		{
			// BYO-SSH: the box survives, so the database really is a choice.
			name: "a BYO-SSH box keeps its database",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderSSH, IsLast: true,
				Server: teardownDone, VM: vmNotApplicable, Revoked: true,
			},
			wantIn:    []string{"server cleaned up", "The database was kept — pass --purge to drop it."},
			wantNotIn: []string{"VM"},
		},
		{
			name: "--purge on a reachable box says the database is gone",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderSSH, IsLast: true,
				Server: teardownDone, Purge: true, Revoked: true,
			},
			wantIn: []string{"The database and its app user were dropped (--purge)."},
		},
		{
			// --purge runs ON the box. If the box did not answer, it did not run.
			name: "--purge against an unreachable box did not happen",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderSSH, IsLast: true,
				Server: teardownFailed, Purge: true,
			},
			wantIn:    []string{"The database was NOT dropped: --purge runs on the box, and the box did not answer."},
			wantNotIn: []string{"were dropped"},
		},
		{
			name: "--skip-server claims no cleanup",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderSSH, IsLast: true,
				Server: teardownSkipped, Revoked: true,
			},
			wantIn:    []string{"the server was left untouched", "local and cloud records removed", "The database was not touched"},
			wantNotIn: []string{"server cleaned up"},
		},
		{
			// A real delete failure IS the case that should send someone to their
			// provider console — and the only one.
			name: "a genuine VM delete failure is called out",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderHetzner, IsLast: true,
				Server: teardownDone, VM: vmDeleteFailed,
			},
			wantIn:    []string{"the hetzner VM could NOT be deleted (see the warning above)"},
			wantNotIn: []string{"no longer exists"},
		},
		{
			name: "--keep-vm says the VM is still there",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderDigitalOcean, IsLast: true,
				Server: teardownDone, VM: vmKept,
			},
			wantIn: []string{"the VM was kept (--keep-vm)", "The database was kept"},
		},
		{
			// Another project still lives on this box: nothing about the box changes.
			name: "a co-tenant project leaves the box alone",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderDigitalOcean, IsLast: false, Server: teardownDone,
			},
			wantIn:    []string{"the box's shared agent stays for its other projects", "the box and its other databases are untouched"},
			wantNotIn: []string{"VM", "last project"},
		},
		{
			// An external database is not on the box at all, so a deleted VM says
			// nothing about it.
			name: "an external database survives a deleted VM",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderDigitalOcean, IsLast: true,
				Server: teardownDone, VM: vmDeleted, ExternalDB: true,
			},
			wantIn:    []string{"The database is external (--db-dsn/--connection) and is untouched"},
			wantNotIn: []string{"went with it"},
		},
		{
			// Interrupted while creating the VM: no box, no agent, no database ever
			// existed, so the summary may only talk about the VM.
			name: "an interrupted provision only talks about the VM",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderDigitalOcean, IsLast: true,
				Provisioning: true, VM: vmDeleted,
			},
			wantIn:    []string{"interrupted while creating the server; the VM was deleted"},
			wantNotIn: []string{"database", "agent"},
		},
		{
			name: "an interrupted provision whose VM never existed",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderDigitalOcean, IsLast: true,
				Provisioning: true, VM: vmNeverCreated,
			},
			wantIn:    []string{"interrupted before the server was created; nothing to clean up"},
			wantNotIn: []string{"database"},
		},
		{
			// The revoke is claimed only when it happened — a record with no agent uuid
			// (a deploy that died before pairing) makes no revoke call at all.
			name: "no revoke is claimed when none happened",
			o: destroyOutcome{
				ID: "app-1", Provider: deploy.ProviderSSH, IsLast: true, Server: teardownDone,
			},
			wantNotIn: []string{"agent revoked"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(tc.o.summary(), "\n")
			for _, want := range tc.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("summary is missing %q\ngot:\n%s", want, got)
				}
			}
			for _, forbidden := range tc.wantNotIn {
				if strings.Contains(got, forbidden) {
					t.Errorf("summary should not mention %q\ngot:\n%s", forbidden, got)
				}
			}
			// Whatever else it says, it always names the deployment it destroyed.
			if !strings.HasPrefix(got, "Destroyed deployment "+tc.o.ID) {
				t.Errorf("summary does not open by naming the deployment:\n%s", got)
			}
		})
	}
}
