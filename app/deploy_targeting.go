package app

import (
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

// pickPriorDeployment returns the most recent recorded deployment for a
// host+identifier, or nil.
//
// A record whose LocalAgentUUID is empty is skipped: the record is written before
// the box finishes pairing, so one without an agent is a deploy that died in
// flight — there is nothing to reuse and nothing to push a schema through.
func pickPriorDeployment(deps []deploy.Deployment, host, identifier string) *deploy.Deployment {
	var match *deploy.Deployment
	for idx := range deps {
		d := deps[idx]
		if d.Host == host && d.Identifier == identifier && d.LocalAgentUUID != "" {
			if match == nil || d.CreatedAt.After(match.CreatedAt) {
				m := d
				match = &m
			}
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
