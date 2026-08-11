package app

import (
	"fmt"
	"strings"

	"github.com/nuzur/nuzur-cli/deploy"
)

// deploy_targeting.go holds the pure half of "which box, which database" — the
// derivations a deploy performs before it touches anything.
//
// They are extracted because `deploy` and `deploy --plan` must reach the SAME
// answer. A plan that resolved a different identifier, database or schema than the
// deploy it is previewing would describe one database and apply to another, which
// is worse than having no plan at all. Sharing these functions is what makes that
// class of bug impossible rather than merely unlikely.

// pickPriorDeployment returns the most recent REUSABLE recorded deployment for a
// host+identifier, or nil.
//
// Reusable means the run that wrote the record got at least as far as pairing an
// agent: a deployment that never did has nothing to reuse and nothing to push a
// schema through. That is now READ off the record — the pairing checkpoint —
// rather than inferred from which fields happen to be filled in. The checkpoint
// is the fact; the empty-LocalAgentUUID heuristic is the FALLBACK, kept because
// it is the only thing a record written before checkpoints existed can be judged
// by (StepRank("") == 0, so such a record is decided entirely by the first arm).
//
// On every record a current CLI writes the two arms agree by construction: the
// pairing checkpoint rides the same write as the uuid it describes, so neither
// can be present without the other. The inference stayed wrong only for the
// window that write closed — and for a record whose uuid was lost by one of the
// wholesale writes that MutateDeployment replaced.
func pickPriorDeployment(deps []deploy.Deployment, host, identifier string) *deploy.Deployment {
	var match *deploy.Deployment
	for idx := range deps {
		d := deps[idx]
		if d.Host != host || d.Identifier != identifier {
			continue
		}
		// "Got far enough to continue from" is per-provider. The agent test below
		// is the VM one: a box is worth adopting once it has an agent, or once
		// the run reached the point of pairing.
		//
		// The k8s path never pairs an agent and never creates a box, so that test
		// can never pass for a run that died before its release — and every such
		// run minted a NEW record instead of writing back to the one for the same
		// cluster and identifier. Five records accumulated for one app in a single
		// afternoon, which also makes `--deployment` ambiguous. There is nothing
		// stranded to be careful about here, so any record for the same target is
		// a legitimate one to continue.
		usable := d.Provider == deploy.ProviderK8s ||
			d.LocalAgentUUID != "" ||
			deploy.StepRank(d.LastCompletedStep) >= deploy.StepRank(deploy.StepAgentPaired)
		if !usable {
			continue
		}
		if match == nil || d.CreatedAt.After(match.CreatedAt) {
			m := d
			match = &m
		}
	}
	return match
}

// pickBoxAgent returns the local-agent UUID already paired on a host (from any
// project's deployment), or "". A box has ONE shared agent serving all its
// projects, so a second project reuses it rather than pairing a new one.
func pickBoxAgent(deps []deploy.Deployment, host string) string {
	var latest *deploy.Deployment
	for idx := range deps {
		d := deps[idx]
		if d.Host == host && d.LocalAgentUUID != "" {
			if latest == nil || d.CreatedAt.After(latest.CreatedAt) {
				m := d
				latest = &m
			}
		}
	}
	if latest == nil {
		return ""
	}
	return latest.LocalAgentUUID
}

// knownAgentUUID is the local agent this deploy already knows it will use, before
// pairing has been confirmed — "" only when nothing on this machine has ever seen
// an agent for this box.
//
// It exists because the deployment record is rewritten wholesale as soon as the box
// exists (step 6b), long before step 12 learns the agent from the pairing wait. A
// re-deploy therefore used to blank a perfectly well-known agent uuid for the whole
// middle of the run. The three sources are the same ones the run itself uses, in the
// same order of authority: the prior record for this host+identifier, the box's
// shared agent (a box has one, serving every project on it), and the record of a box
// being adopted after its own deploy died in flight.
func knownAgentUUID(prior *deploy.Deployment, boxAgentUUID string, adopted *deploy.Deployment) string {
	priorAgent := ""
	if prior != nil {
		priorAgent = prior.LocalAgentUUID
	}
	adoptedAgent := ""
	if adopted != nil {
		adoptedAgent = adopted.LocalAgentUUID
	}
	return firstNonEmpty(priorAgent, boxAgentUUID, adoptedAgent)
}

// boxAction is what a deploy does about the SERVER it needs, before anything is
// generated, provisioned or billed.
type boxAction int

const (
	// boxUseGivenHost: --provider ssh. The user named the box; nothing is created
	// and nothing is inferred. This is also how a DIFFERENT project lands on a box
	// that already hosts one — explicit --host, multi-project co-tenancy intact.
	boxUseGivenHost boxAction = iota
	// boxProvision: ask the managed provider for a fresh VM. This is the arm that
	// costs money, so it is never taken silently.
	boxProvision
	// boxReuseRecorded: this project + identifier was already deployed to a box
	// this provider created for us. Skip provisioning and bootstrap that box (the
	// bootstrap is idempotent), exactly as if the user had passed
	// --provider ssh --host <recorded>.
	boxReuseRecorded
	// boxFail: a record exists that neither describes a reusable box nor can be
	// ignored — provisioning past it would leak a VM.
	boxFail
)

// boxDecisionInput is everything the server decision needs, and nothing that
// costs a network call. Deployment records are passed in rather than read so the
// policy stays pure and table-testable.
type boxDecisionInput struct {
	Provider    deploy.Provider // --provider, already defaulted to ssh
	HostFlag    string          // --host
	NewVM       bool            // --new-vm
	Identifier  string          // the resolved deployment identifier
	ProjectUUID string
	Deployments []deploy.Deployment
}

// boxDecision is the answer: which box, and what to say about it.
type boxDecision struct {
	Action boxAction
	// Host/User/Port describe the recorded box on a boxReuseRecorded. They are the
	// RECORDED SSH parameters, so the reuse connects the same way the deploy that
	// created the box did.
	Host string
	User string
	Port int
	// Record is the deployment record the decision was taken from (the reused box,
	// or the existing box a boxProvision is billing alongside). nil when there is
	// no relevant record.
	Record *deploy.Deployment
	// Message is printed before acting — the line whose absence made every managed
	// re-deploy create and bill for a second VM in silence.
	Message string
}

// pickManagedBox returns the most recent recorded deployment of this project +
// identifier whose box a MANAGED provider created for us, or nil.
//
// Deliberately different from pickPriorDeployment in two ways:
//
//   - It matches on project + identifier, not host + identifier. A managed deploy
//     passes no --host (the provider hands one out), so the host-keyed lookup could
//     never match on a re-deploy and every run provisioned a new droplet.
//   - It does NOT skip records with an empty LocalAgentUUID. Such a record is a
//     deploy that died in flight, which is precisely the case where a VM may exist
//     and be billing with nothing adopting it. Whether that box is usable is
//     decided by trying to reach it, not by whether pairing got that far.
//
// BYO-SSH records are excluded: that box is the user's, not something this CLI
// created, and a `--provider digitalocean` run asking for a managed VM should not
// silently land on it.
func pickManagedBox(deps []deploy.Deployment, projectUUID, identifier string) *deploy.Deployment {
	var match *deploy.Deployment
	for idx := range deps {
		d := deps[idx]
		if d.Identifier != identifier {
			continue
		}
		// An empty ProjectUUID is a record written before the field existed; treat
		// it as belonging to whoever is asking rather than stranding the box.
		if d.ProjectUUID != "" && projectUUID != "" && d.ProjectUUID != projectUUID {
			continue
		}
		if !d.Provider.CreatesInfrastructure() {
			continue
		}
		if match == nil || d.CreatedAt.After(match.CreatedAt) {
			m := d
			match = &m
		}
	}
	return match
}

// decideDeployBox picks the server a deploy runs against.
//
// The bug this exists for: `deploy --provider digitalocean --identifier x` created
// a NEW droplet, record, agent, connection, database and JWT key on every run,
// because the re-deploy lookup was keyed on --host and a managed deploy has no
// --host to key on. Three re-deploys meant three billing droplets and not one line
// of output saying a server had been created rather than reused.
//
// The matrix:
//
//	provider  record for project+identifier        --new-vm  →
//	ssh       (irrelevant)                         (ignored)   use --host as given
//	managed   none                                 no          provision, say so
//	managed   none                                 yes         provision, say so
//	managed   same provider, has a host            no          REUSE that host
//	managed   same provider, has a host            yes         provision, warn: two boxes now bill
//	managed   same provider, mid-provision, no host no         FAIL: a VM may exist and be unfindable from here
//	managed   same provider, mid-provision, no host yes        provision, warn about the orphan
//	managed   a DIFFERENT managed provider         no          provision, warn: the other box still bills
//
// Reuse deliberately does not care whether the recorded deploy finished pairing:
// an unfinished one left a box behind too, and the bootstrap is idempotent. What
// it does care about is whether the box answers — decided by the caller, which can
// reach the network; an unreachable box takes reusedBoxUnreachableError below.
//
// Checkpoints changed what the messages may SAY, and nothing about the matrix.
// The three arms that speak about a half-finished deployment — reuse, --new-vm
// and the mid-provision failure — append lastRunFact, which reads how far the
// last run got off the record instead of guessing it from an empty field. A
// record with nothing recorded produces no sentence, so its message is what it
// always was.
func decideDeployBox(in boxDecisionInput) (boxDecision, error) {
	if in.Provider.UsesGivenHost() {
		return boxDecision{Action: boxUseGivenHost, Host: in.HostFlag}, nil
	}

	rec := pickManagedBox(in.Deployments, in.ProjectUUID, in.Identifier)
	provider := string(in.Provider)

	// No memory of this project+identifier on a managed provider: a first deploy.
	// Unchanged behaviour, except that it now says a VM is being created.
	if rec == nil {
		return boxDecision{
			Action: boxProvision,
			Message: fmt.Sprintf(
				"Creating a new %s VM for identifier %q — no previous deployment of this project under that identifier is recorded on this machine, so a new server will be created and will bill.",
				provider, in.Identifier),
		}, nil
	}

	// A record on a different managed provider is not reusable, but the box behind
	// it is still running. Say so rather than quietly doubling the bill.
	if rec.Provider != in.Provider {
		return boxDecision{
			Action: boxProvision,
			Record: rec,
			Message: fmt.Sprintf(
				"Creating a new %s VM for identifier %q: the recorded server for this identifier is on %s (deployment %s%s), which --provider %s cannot reuse. That server keeps running and billing — remove it with `nuzur-cli destroy %s`.",
				provider, in.Identifier, rec.Provider, rec.ID, hostSuffix(rec.Host), provider, rec.ID),
		}, nil
	}

	if in.NewVM {
		// Deliberately conditional. This message used to assert that the recorded box
		// "already runs it" and that "Both servers bill" — neither of which the decision
		// is in a position to know, and both of which are false in the case --new-vm
		// exists for: the CLI has just refused a re-deploy because that box did not
		// answer, and told the user to re-run with --new-vm. Reaching the box to find
		// out costs an SSH round trip this decision does not make (it is taken before
		// anything touches the network), so the honest form states the condition
		// instead of guessing which side of it we are on.
		where := fmt.Sprintf("deployment %s%s is already recorded for it", rec.ID, hostSuffix(rec.Host))
		if strings.TrimSpace(rec.Host) == "" {
			where = fmt.Sprintf("deployment %s was left mid-provision and may have leaked a VM", rec.ID)
		}
		return boxDecision{
			Action: boxProvision,
			Record: rec,
			Message: fmt.Sprintf(
				"--new-vm: creating a NEW %s VM for identifier %q even though %s.%s If that server still exists it keeps billing alongside this one — check with `nuzur-cli destroy %s`, which removes it, or your %s console.",
				provider, in.Identifier, where, spaced(lastRunFact(rec)), rec.ID, provider),
		}, nil
	}

	// A record with no host is a deploy that died between reserving the VM's name
	// and learning its address (see Deployment.Provisioning). There is nothing to
	// reuse and nothing to reach, and provisioning past it would leave a VM that
	// only `destroy` — which can still resolve it by the reserved name — can find.
	if strings.TrimSpace(rec.Host) == "" {
		return boxDecision{Action: boxFail, Record: rec}, fmt.Errorf(
			"deployment %s for identifier %q was left mid-provision on %s and never recorded a host, so there is no server to reuse and a VM may already exist and be billing.%s\n"+
				"Run `nuzur-cli destroy %s` to remove it (destroy can still find the VM by the name it reserved), or re-run with --new-vm to provision a fresh server and deal with that one yourself.",
			rec.ID, in.Identifier, provider, spaced(lastRunFact(rec)), rec.ID)
	}

	return boxDecision{
		Action: boxReuseRecorded,
		Host:   rec.Host,
		User:   rec.User,
		Port:   rec.Port,
		Record: rec,
		Message: fmt.Sprintf(
			"Reusing the existing %s server %s for identifier %q (deployment %s) — no new VM will be created.%s Pass --new-vm to provision a fresh one instead.",
			provider, rec.Host, in.Identifier, rec.ID, spaced(lastRunFact(rec))),
	}, nil
}

// lastRunFact is what the RECORD says about the run that wrote it: how far it
// got, and what it stopped on. One sentence, ready to drop into a message about
// reusing or provisioning past that record — or "" when there is nothing
// recorded to say.
//
// It exists because those messages could previously only INFER the same thing
// from which fields happened to be empty ("no host, so it died mid-provision"),
// which is the inference that made a re-deploy mint a second record for one box.
// The messages keep their inferred phrasing — the matrix and its wording are
// pinned — and this states the recorded fact alongside it.
//
// Returns "" in exactly two cases, both of which mean the fact would be noise:
//
//   - the record carries neither a checkpoint nor an error. It predates
//     checkpoints, or its run died before reaching one; either way the
//     empty-field reading is all there is, and those records keep today's
//     wording byte for byte.
//   - the record is FINALIZED and clean. Nothing died; a re-deploy of a healthy
//     box should not be told how its last run went.
func lastRunFact(rec *deploy.Deployment) string {
	if rec == nil {
		return ""
	}
	step := strings.TrimSpace(rec.LastCompletedStep)
	lastErr := strings.TrimSpace(rec.LastError)
	if step == "" && lastErr == "" {
		return ""
	}
	if lastErr == "" && deploy.StepRank(step) >= deploy.StepRank(deploy.StepFinalized) {
		return ""
	}
	lastErr = endSentence(firstLine(lastErr))
	switch {
	case step == "":
		return "The last run of this deployment failed before it recorded any step: " + lastErr
	case lastErr == "":
		return fmt.Sprintf("The last run of this deployment died after '%s'.", step)
	default:
		return fmt.Sprintf("The last run of this deployment died after '%s': %s", step, lastErr)
	}
}

// firstLine keeps a recorded error to ONE line.
//
// LastError is whatever the run ended with, and this CLI's own errors are often
// several lines — a diagnosis followed by the ways forward. Pasted whole into
// the middle of another sentence, that is exactly what it reads like. The record
// keeps the full text, which is what to read when this line is not enough.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx]) + " …"
	}
	return s
}

// endSentence terminates a recorded error so it can be read as prose. Errors are
// conventionally written without a full stop, and one that already ends in a
// terminator — including the ellipsis firstLine leaves — does not want a second.
func endSentence(s string) string {
	if s == "" {
		return ""
	}
	for _, end := range []string{".", "!", "?", "…", ":"} {
		if strings.HasSuffix(s, end) {
			return s
		}
	}
	return s + "."
}

// spaced prefixes a non-empty sentence with the space that separates it from the
// one before. The empty case is the point: a record with nothing recorded
// produces no sentence AND no stray space, so its message is byte-identical to
// what it was before checkpoints existed.
func spaced(sentence string) string {
	if sentence == "" {
		return ""
	}
	return " " + sentence
}

// hostSuffix renders " on <host>" for a record that has one, and nothing for the
// mid-provision records that don't — so a message never reads "on ".
func hostSuffix(host string) string {
	if h := strings.TrimSpace(host); h != "" {
		return " on " + h
	}
	return ""
}

// reusedBoxUnreachableError is what a managed re-deploy says when the box it was
// going to reuse doesn't answer.
//
// It cannot fall back to provisioning: the whole point of the reuse is that a
// second VM is a real, recurring cost the user did not ask for. So it stops, and
// spells out the three ways forward rather than leaving the user to guess which
// one this is.
func reusedBoxUnreachableError(rec *deploy.Deployment, provider deploy.Provider, identifier string, cause error) error {
	return fmt.Errorf(
		"the recorded %s server for identifier %q (deployment %s, host %s) is not reachable over SSH: %w\n"+
			"A managed re-deploy reuses the recorded server instead of creating a second VM, so this deploy cannot continue. Either:\n"+
			"  - restore connectivity to %s (the VM may be powered off, or its firewall/SSH key may have changed), or\n"+
			"  - remove the stale record and its VM with `nuzur-cli destroy %s`, or\n"+
			"  - re-run with --new-vm to provision a fresh server (which bills for a second one).",
		provider, identifier, rec.ID, rec.Host, cause, rec.Host, rec.ID)
}

// applyDeploymentSelector adopts a recorded deployment's targeting into the settings
// a real deploy runs from, so `--deployment <id>` selects a box the way it already
// selects a database for --plan.
//
// It exists because --plan's "you can paste this" command was unrunnable. The record
// carries the project, the provider, the host and the identifier — which is why the
// user never typed them — and a suggestion that drops all four either fails outright
// or, on a TTY, prompts for a project and applies to something else entirely while
// carrying --allow-destructive. The flag's own help calls it "the most reliable
// selector"; this makes that true of `deploy` and not only of `deploy --plan`.
//
// Only fills what the user did not state: an explicit flag always wins, so the record
// is a default, never an override. Returns what it adopted, for the caller to print —
// targeting resolved in silence is how a deploy lands somewhere its operator did not
// expect.
func applyDeploymentSelector(s *deploySettings, rec *deploy.Deployment, isSet func(string) bool) ([]string, error) {
	if rec == nil {
		return nil, nil
	}
	var adopted []string
	take := func(flag, name, from string, assign func()) {
		if isSet(flag) || strings.TrimSpace(from) == "" {
			return
		}
		assign()
		adopted = append(adopted, name+"="+from)
	}
	take("project", "project", rec.ProjectUUID, func() { s.Project = rec.ProjectUUID })
	take("identifier", "identifier", rec.Identifier, func() { s.Identifier = rec.Identifier })
	take("provider", "provider", string(rec.Provider), func() { s.Provider = string(rec.Provider) })
	take("host", "host", rec.Host, func() { s.Host = rec.Host })
	take("user", "user", rec.User, func() { s.User = rec.User })
	take("db", "db", string(rec.DBEngine), func() { s.DB = string(rec.DBEngine) })
	take("domain", "domain", rec.Domain, func() { s.Domain = rec.Domain })
	// The other two hostnames, adopted exactly like the first. Forgetting one is
	// not a cosmetic omission: on k8s an unstated host writes no ingress block,
	// the chart defaults to `ingress.enabled: false`, and helm deletes the live
	// Ingress. See Deployment.AuthDomain.
	take("auth-domain", "auth-domain", rec.AuthDomain, func() { s.AuthDomain = rec.AuthDomain })
	take("grpc-domain", "grpc-domain", rec.GRPCDomain, func() { s.GRPCDomain = rec.GRPCDomain })
	// The image REPOSITORY, not the whole reference: the tag belongs to this
	// deploy (it is derived from the commit being released), while the registry
	// and repository are a property of the project that does not change between
	// releases. Recorded as one string, so it is split back out here.
	//
	// Without this a re-deploy of a recorded deployment failed at `resolve image`
	// with "cannot tell which image to deploy", even though the record held the
	// exact image the last release ran — every other selector on the record is
	// adopted, and this one being missed made the record look incomplete when it
	// was not.
	if repo := imageRepoFromRef(rec.ImageRef); repo != "" {
		take("image-repo", "image-repo", repo, func() { s.ImageRepo = repo })
	}
	take("source-dir", "source-dir", rec.WorkspaceDir, func() { s.SourceDir = rec.WorkspaceDir })
	// The project VERSION this deployment last shipped.
	//
	// Every other selector on the record is adopted, and this one was not, so a
	// re-deploy of a fully recorded deployment refused to start at all: "a project
	// version is required in non-interactive mode; pass --version <identifier|uuid>"
	// — about a record that names the version on the line above the one being read.
	//
	// Adopting it is also the right default rather than merely a convenient one: a
	// re-deploy means "ship this deployment again", and silently moving it to
	// whatever version is newest is a bigger decision than the command expressed.
	// --version still overrides, which is how you move it forward.
	take("version", "version", rec.ProjectVersionUUID, func() { s.Version = rec.ProjectVersionUUID })
	// The team connection this deployment runs against. A uuid, not a credential:
	// it resolves server-side against the user's own teams, so adopting it is the
	// same kind of act as adopting the host.
	take("connection", "connection", rec.TeamConnUUID, func() { s.Connection = rec.TeamConnUUID })

	// An external database that this run still cannot reach is the one refusal
	// worth making: adopting everything else and silently self-hosting a NEW,
	// empty database on the box is the outcome here that destroys something.
	//
	// Checked AFTER adoption, not before. The record now stores which team
	// connection it was deployed against, so for most external deployments the
	// line above has already answered the question — and refusing first turned a
	// re-deploy that the record could fully describe into a dead end whose only
	// instruction was "remember what you typed last time".
	if rec.ExternalDB && strings.TrimSpace(s.DBDSN) == "" && strings.TrimSpace(s.Connection) == "" {
		return nil, fmt.Errorf(
			"deployment %s runs against an EXTERNAL database and neither this run nor its record says how to reach it "+
				"(the record stores that the database is external, and never its credentials) — re-run with the "+
				"--db-dsn or --connection you deployed it with. Without one, this deploy would self-host a new, "+
				"empty database on the box instead",
			rec.ID)
	}
	if !isSet("port") && rec.Port != 0 {
		s.Port = rec.Port
		adopted = append(adopted, fmt.Sprintf("port=%d", rec.Port))
	}
	return adopted, nil
}

// deploySchemaName derives the schema that the diff engine, the data-manager deep
// link and the agent connection's default schema all target: in MySQL the database
// IS the schema; in Postgres a database contains schemas, defaulting to `public`.
func deploySchemaName(engine deploy.DBEngine, dbName, dbSchemaFlag string) string {
	if engine == deploy.DBPostgres {
		return firstNonEmpty(dbSchemaFlag, "public")
	}
	return dbName
}

// planIdentifier derives the deployment identifier — which names the database, the
// service and the config on the box — from --identifier, else the identifier in a
// go-code-gen config, else the sanitized project name.
//
// runDeploy passes the config it just resolved; --plan passes the project's
// last-used saved config, because resolving the real one would run the generator's
// config machinery for a command that is supposed to touch nothing. Those two
// agree in every case that matters: the resolved config's identifier either came
// from --identifier (checked first here) or from that same saved config.
func planIdentifier(flagIdentifier string, goCodeGenConfig map[string]interface{}, projectName string) string {
	return firstNonEmpty(flagIdentifier, stringValue(goCodeGenConfig, "identifier", ""), sanitizeDBName(projectName))
}
