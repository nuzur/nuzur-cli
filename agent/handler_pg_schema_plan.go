package agent

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lib/pq"
	pgschemadiff "github.com/nuzur/pg-schema-diff/pkg/diff"
	pgschemaschema "github.com/nuzur/pg-schema-diff/pkg/schema"
	"github.com/nuzur/pg-schema-diff/pkg/tempdb"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// nuzurManagedObjectTypes is the set of schema object classes a project version
// can express, and therefore the only classes a diff against the local database
// may consider.
//
// Keep in sync with nuzurManagedObjectTypes() in nuzur-go's
// connection-manager/module/sql-diff-manager and with
// TestObjectTypeAllowlistAgainstLiveDatabase in pg-schema-diff, which is what
// catches drift.
func nuzurManagedObjectTypes() []pgschemaschema.ObjectType {
	return []pgschemaschema.ObjectType{
		pgschemaschema.ObjectTypeNamedSchema,
		pgschemaschema.ObjectTypeTable,
		pgschemaschema.ObjectTypeIndex,
		pgschemaschema.ObjectTypeForeignKeyConstraint,
	}
}

// handleComputePgSchemaPlan computes a Postgres schema-diff plan ON-BOX. The
// cloud can't drive pg-schema-diff's tempdb factory over the agent tunnel (it
// needs a real second database + a raw *sql.DB), so instead the cloud ships the
// two rendered create.sql sources and we run pg-schema-diff locally against a
// throwaway temp database on this Postgres instance, returning the apply SQL.
//
// This is the Postgres counterpart to the cloud-side tempdb factory used for
// remote connections. It requires the registered role to have CREATEDB (the
// deploy bootstrap grants it) so temp databases can be created here.
func handleComputePgSchemaPlan(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, req *pb.ComputePgSchemaPlanRequest) {
	connUUID := req.GetLocalAgentConnectionUuid()
	entry, ok := pool.registry.FindByUUID(connUUID)
	if !ok {
		sendQueryError(stream, req.GetRequestId(), fmt.Sprintf("no local connection registered for uuid %s", connUUID))
		return
	}
	if entry.Driver != "postgres" {
		sendQueryError(stream, req.GetRequestId(), fmt.Sprintf("compute pg schema plan requires a postgres connection, got %q", entry.Driver))
		return
	}
	if entry.DSN == "" {
		sendQueryError(stream, req.GetRequestId(), fmt.Sprintf("connection %q has no DSN in the OS keychain", entry.Name))
		return
	}
	baseDSN, err := pgKeywordDSN(entry.DSN)
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), fmt.Sprintf("parsing connection DSN: %v", err))
		return
	}

	schema := req.GetSchema()
	if schema == "" {
		schema = "public"
	}

	// An empty "existing" side is never a legitimate request: it would diff
	// against nothing and return a plan that recreates the whole schema. Refuse
	// loudly rather than hand back something catastrophic.
	if !req.GetUseLiveSchemaSource() && req.GetExistingCreateSql() == "" {
		sendQueryError(stream, req.GetRequestId(), "compute pg schema plan: existing_create_sql is empty and use_live_schema_source is not set")
		return
	}

	applySQL, err := computePgPlan(ctx, baseDSN, schema, req.GetExistingCreateSql(), req.GetNewCreateSql(), req.GetUseLiveSchemaSource())
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}

	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_ComputePgSchemaPlanResponse{
		ComputePgSchemaPlanResponse: &pb.ComputePgSchemaPlanResponse{
			RequestId: req.GetRequestId(),
			ApplySql:  applySQL,
		},
	}})
}

// computePgPlan runs pg-schema-diff against temp databases created on the same
// local instance as baseDSN, returning the ordered apply DDL.
//
// When useLive is set the "existing" side is read straight out of the local
// database — this process is the one with real access to it, so there is nothing
// to reconstruct and nothing to lose in the reconstruction. existingSQL is then
// ignored, and only the target schema needs staging, so the run creates one temp
// database instead of two.
func computePgPlan(ctx context.Context, baseDSN, schema, existingSQL, newSQL string, useLive bool) (string, error) {
	newDir, err := writeCreateSQLDir("nuzur-pgdiff-new-", newSQL)
	if err != nil {
		return "", fmt.Errorf("staging new schema: %w", err)
	}
	defer os.RemoveAll(newDir)

	newSource, err := pgschemadiff.DirSchemaSource([]string{newDir})
	if err != nil {
		return "", fmt.Errorf("reading new schema: %w", err)
	}

	var existingSource pgschemadiff.SchemaSource
	if useLive {
		// No search_path pinning here: every introspection query pg-schema-diff
		// runs is pg_catalog-qualified and the diff is scoped by
		// WithIncludeSchemas below, so setting it on a connection to the user's
		// real database would be a side effect for no benefit. The temp-db
		// factory still pins it, because that side applies unqualified DDL.
		liveDB, err := sql.Open("postgres", baseDSN)
		if err != nil {
			return "", fmt.Errorf("opening local database: %w", err)
		}
		liveDB.SetMaxOpenConns(10)
		liveDB.SetMaxIdleConns(2)
		defer liveDB.Close()
		if err := liveDB.PingContext(ctx); err != nil {
			return "", fmt.Errorf("reaching local database: %w", err)
		}
		existingSource = pgschemadiff.DBSchemaSource(liveDB)
	} else {
		existingDir, err := writeCreateSQLDir("nuzur-pgdiff-existing-", existingSQL)
		if err != nil {
			return "", fmt.Errorf("staging existing schema: %w", err)
		}
		defer os.RemoveAll(existingDir)

		existingSource, err = pgschemadiff.DirSchemaSource([]string{existingDir})
		if err != nil {
			return "", fmt.Errorf("reading existing schema: %w", err)
		}
	}

	// pg-schema-diff opens a "root" database purely to issue CREATE/DROP DATABASE
	// for the temp databases, and defaults it to one named "postgres". Not every
	// server has one — DigitalOcean Managed Postgres calls its maintenance
	// database "defaultdb" — so the default fails with `database "postgres" does
	// not exist` before the diff starts, and no schema can ever be applied. The
	// factory only needs *a* database it can connect to, and this connection
	// already names one: use it, as pg-schema-diff's own CLI does.
	rootDatabase := pgDatabaseName(baseDSN)
	if rootDatabase == "" {
		// Nothing better to fall back on than the library's default.
		rootDatabase = "postgres"
	}

	var tempDBs []*sql.DB
	factory, err := tempdb.NewOnInstanceFactory(ctx, func(ctx context.Context, dbName string) (*sql.DB, error) {
		dsn := swapPGDatabase(baseDSN, dbName)
		// For temp databases, pin search_path to the target schema so the applied
		// (unqualified) DDL lands there and the diff scope (WithIncludeSchemas)
		// matches — the same thing the remote tempdb path does via its DSN. The
		// root db is only used to CREATE/DROP databases, so leave it alone (it is
		// the user's own database now, not a scratch one).
		isTemp := dbName != rootDatabase
		if isTemp && schema != "" {
			dsn += " search_path=" + schema
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return nil, err
		}
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
		// Materialize the target schema in the temp DB (search_path alone doesn't
		// create it; unqualified DDL would fail if the schema is missing).
		if isTemp && schema != "" {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", pq.QuoteIdentifier(schema))); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
		tempDBs = append(tempDBs, db)
		return db, nil
	}, tempdb.WithRootDatabase(rootDatabase))
	if err != nil {
		return "", fmt.Errorf("creating temp-db factory: %w", err)
	}
	defer func() {
		_ = factory.Close()
		for _, db := range tempDBs {
			_ = db.Close()
		}
	}()

	plan, err := pgschemadiff.Generate(ctx,
		existingSource,
		newSource,
		pgschemadiff.WithTempDbFactory(factory),
		pgschemadiff.WithIncludeSchemas(schema),
		pgschemadiff.WithDoNotValidatePlan(),
		// Restrict both sides to the object classes a project version can
		// express. Without this, everything else the local database contains —
		// extensions, enums, sequences, functions, triggers, check constraints,
		// policies, replica identity, partitioning — looks like something to
		// drop the moment the existing side becomes a real database.
		pgschemadiff.WithGetSchemaOpts(pgschemaschema.WithIncludeObjectTypes(nuzurManagedObjectTypes()...)),
	)
	if err != nil {
		return "", fmt.Errorf("generating schema diff: %w", err)
	}

	var b strings.Builder
	for _, stmt := range plan.Statements {
		b.WriteString(stmt.ToSQL())
		b.WriteString("\n")
	}
	return b.String(), nil
}

// writeCreateSQLDir writes the given SQL to a create.sql file in a fresh temp
// dir so pg-schema-diff's DirSchemaSource can read it.
func writeCreateSQLDir(prefix, sqlBody string) (string, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "create.sql"), []byte(sqlBody), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// pgKeywordDSN normalizes a Postgres DSN to lib/pq keyword form (host=... dbname=...)
// so swapPGDatabase can rewrite the target database. URL-form DSNs (postgres://…)
// are converted; keyword-form DSNs pass through unchanged.
func pgKeywordDSN(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return pq.ParseURL(dsn)
	}
	return dsn, nil
}

// pgDatabaseName reads the dbname out of a lib/pq keyword DSN. Empty when the
// DSN names no database. Same assumption as swapPGDatabase: no spaces inside
// values.
func pgDatabaseName(keywordDSN string) string {
	for _, f := range strings.Fields(keywordDSN) {
		if name, ok := strings.CutPrefix(f, "dbname="); ok {
			return name
		}
	}
	return ""
}

// swapPGDatabase rewrites the dbname of a lib/pq keyword DSN to target, so the
// tempdb factory can connect to the root database and to each newly created
// temp database. Assumes no spaces inside values (true for our generated DSNs
// and for pq.ParseURL output).
func swapPGDatabase(keywordDSN, target string) string {
	fields := strings.Fields(keywordDSN)
	out := make([]string, 0, len(fields)+1)
	replaced := false
	for _, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			out = append(out, "dbname="+target)
			replaced = true
			continue
		}
		out = append(out, f)
	}
	if !replaced {
		out = append(out, "dbname="+target)
	}
	return strings.Join(out, " ")
}
