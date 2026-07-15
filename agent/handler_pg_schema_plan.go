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
	"github.com/nuzur/pg-schema-diff/pkg/tempdb"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

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

	applySQL, err := computePgPlan(ctx, baseDSN, schema, req.GetExistingCreateSql(), req.GetNewCreateSql())
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
func computePgPlan(ctx context.Context, baseDSN, schema, existingSQL, newSQL string) (string, error) {
	existingDir, err := writeCreateSQLDir("nuzur-pgdiff-existing-", existingSQL)
	if err != nil {
		return "", fmt.Errorf("staging existing schema: %w", err)
	}
	defer os.RemoveAll(existingDir)
	newDir, err := writeCreateSQLDir("nuzur-pgdiff-new-", newSQL)
	if err != nil {
		return "", fmt.Errorf("staging new schema: %w", err)
	}
	defer os.RemoveAll(newDir)

	existingSource, err := pgschemadiff.DirSchemaSource([]string{existingDir})
	if err != nil {
		return "", fmt.Errorf("reading existing schema: %w", err)
	}
	newSource, err := pgschemadiff.DirSchemaSource([]string{newDir})
	if err != nil {
		return "", fmt.Errorf("reading new schema: %w", err)
	}

	var tempDBs []*sql.DB
	factory, err := tempdb.NewOnInstanceFactory(ctx, func(ctx context.Context, dbName string) (*sql.DB, error) {
		dsn := swapPGDatabase(baseDSN, dbName)
		// For temp databases, pin search_path to the target schema so the applied
		// (unqualified) DDL lands there and the diff scope (WithIncludeSchemas)
		// matches — the same thing the remote tempdb path does via its DSN. The
		// root "postgres" db is only used to CREATE/DROP databases, so leave it.
		isTemp := dbName != "postgres"
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
	})
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

// swapPGDatabase rewrites the dbname of a lib/pq keyword DSN to target, so the
// tempdb factory can connect to the root "postgres" database and to each newly
// created temp database. Assumes no spaces inside values (true for our generated
// DSNs and for pq.ParseURL output).
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
