package agent

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// rowsChunkMaxRows caps how many rows we batch into a single RowsChunk before
// streaming it. Tuned for "interesting query result, not huge" — keeps chunk
// proto small enough for gRPC's default 4MiB frame even with wide rows.
const rowsChunkMaxRows = 1000

// queryer is the small slice of *sqlx.DB / *sqlx.Tx that handleRunQuery and
// handleExec need. Both methods exist on either type so handlers can stay
// agnostic to whether they're in a transaction.
type queryer interface {
	QueryxContext(ctx context.Context, query string, args ...interface{}) (*sqlx.Rows, error)
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// handleRunQuery is the SELECT-style execution path. It streams RowsChunk
// messages back as rows are scanned so the cloud can start consuming before
// the agent has the full result in memory.
func handleRunQuery(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, txs *txPool, req *pb.RunQueryRequest) {
	q, release, err := resolveQueryerWithSchema(ctx, pool, txs, req.GetLocalAgentConnectionUuid(), req.GetTxId(), req.GetSchema())
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	defer release()

	args := stringArgsToAny(req.GetArgs())
	rows, err := q.QueryxContext(ctx, req.GetSql(), args...)
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	cts, _ := rows.ColumnTypes()
	columns := buildColumnMetadata(cols, cts)

	dest := make([]interface{}, len(cols))
	for i := range dest {
		var v interface{}
		dest[i] = &v
	}

	batch := make([]*pb.Row, 0, rowsChunkMaxRows)
	emitChunk := func(more bool) {
		chunk := &pb.RowsChunk{
			RequestId: req.GetRequestId(),
			Rows:      batch,
			RowCount:  int64(len(batch)),
			More:      more,
		}
		if columns != nil {
			chunk.Columns = columns
			columns = nil // only attached to the first chunk
		}
		_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_RowsChunk{RowsChunk: chunk}})
		batch = batch[:0]
	}

	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			sendQueryError(stream, req.GetRequestId(), err.Error())
			return
		}
		row := &pb.Row{Values: make([]*pb.Value, len(dest))}
		for i, d := range dest {
			row.Values[i] = encodeValue(*(d.(*interface{})))
		}
		batch = append(batch, row)
		if len(batch) >= rowsChunkMaxRows {
			emitChunk(true)
		}
	}
	if err := rows.Err(); err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	emitChunk(false)
}

// handleExec runs non-row-returning SQL. INSERT / UPDATE / DELETE / DDL.
func handleExec(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, txs *txPool, req *pb.ExecRequest) {
	q, release, err := resolveQueryerWithSchema(ctx, pool, txs, req.GetLocalAgentConnectionUuid(), req.GetTxId(), req.GetSchema())
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	defer release()

	args := stringArgsToAny(req.GetArgs())
	res, err := q.ExecContext(ctx, req.GetSql(), args...)
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	rowsAffected, _ := res.RowsAffected() // some drivers don't support it; ignore err
	lastID, _ := res.LastInsertId()       // not meaningful for Postgres; ignore err
	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_ExecResponse{
		ExecResponse: &pb.ExecResponse{
			RequestId:    req.GetRequestId(),
			RowsAffected: rowsAffected,
			LastInsertId: lastID,
		},
	}})
}

// handleBeginTx opens a fresh *sqlx.Tx against the requested local connection
// and stores it in the tx pool, returning the agent-side tx_id. If schema is
// set, we apply it (USE / SET search_path) inside the tx so all subsequent
// queries on this tx_id inherit the schema without each having to repeat it.
func handleBeginTx(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, txs *txPool, req *pb.BeginTxRequest) {
	db, err := pool.Get(req.GetLocalAgentConnectionUuid())
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	if schema := req.GetSchema(); schema != "" {
		if err := applySchemaToExecer(ctx, tx, db.DriverName(), schema); err != nil {
			_ = tx.Rollback()
			sendQueryError(stream, req.GetRequestId(), err.Error())
			return
		}
	}
	txID := txs.Add(tx)
	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_BeginTxResponse{
		BeginTxResponse: &pb.BeginTxResponse{
			RequestId: req.GetRequestId(),
			TxId:      txID,
		},
	}})
}

func handleCommit(stream pb.NuzurConnectionManager_LocalAgentChannelClient, txs *txPool, req *pb.CommitRequest) {
	tx, ok := txs.Pop(req.GetTxId())
	if !ok {
		sendQueryError(stream, req.GetRequestId(), "unknown tx_id (possibly idle-reaped)")
		return
	}
	if err := tx.Commit(); err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_CommitResponse{
		CommitResponse: &pb.CommitResponse{RequestId: req.GetRequestId()},
	}})
}

func handleRollback(stream pb.NuzurConnectionManager_LocalAgentChannelClient, txs *txPool, req *pb.RollbackRequest) {
	tx, ok := txs.Pop(req.GetTxId())
	if !ok {
		// Idempotent — if the tx is already gone, report success so the cloud
		// caller doesn't loop on a no-op cleanup.
		_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_RollbackResponse{
			RollbackResponse: &pb.RollbackResponse{RequestId: req.GetRequestId()},
		}})
		return
	}
	if err := tx.Rollback(); err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_RollbackResponse{
		RollbackResponse: &pb.RollbackResponse{RequestId: req.GetRequestId()},
	}})
}

// resolveQueryerWithSchema picks the right backend to run the SQL against,
// applying the requested schema if any. Resolution:
//   - tx_id non-empty: use the open *sqlx.Tx. Schema was already applied at
//     BeginTx and persists for the whole tx — we ignore the schema param here
//     to avoid stomping it (and to skip redundant USE work).
//   - tx_id empty + schema empty: use the pool *sqlx.DB directly.
//   - tx_id empty + schema set: acquire a single *sqlx.Conn from the pool,
//     apply `USE <schema>` (mysql) or `SET search_path TO <schema>` (postgres),
//     and run the actual SQL on that conn. The returned release closure
//     hands the conn back to the pool when the caller is done.
func resolveQueryerWithSchema(ctx context.Context, pool *dbPool, txs *txPool, connUUID, txID, schema string) (queryer, func(), error) {
	noop := func() {}
	if txID != "" {
		tx, ok := txs.Get(txID)
		if !ok {
			return nil, noop, &resolveErr{msg: "unknown tx_id (possibly idle-reaped)"}
		}
		return tx, noop, nil
	}
	db, err := pool.Get(connUUID)
	if err != nil {
		return nil, noop, err
	}
	if schema == "" {
		return db, noop, nil
	}
	conn, err := db.Connx(ctx)
	if err != nil {
		return nil, noop, fmt.Errorf("acquire conn for schema apply: %w", err)
	}
	if err := applySchemaToExecer(ctx, conn, db.DriverName(), schema); err != nil {
		_ = conn.Close()
		return nil, noop, err
	}
	return conn, func() { _ = conn.Close() }, nil
}

// applySchemaToExecer pins the active database/schema on the given conn or tx
// for the duration of its lifetime. mysql uses `USE`; postgres uses
// `SET search_path TO`. Identifiers are quoted to neutralize anything funky
// in the schema name; we still error out if the underlying server rejects it.
func applySchemaToExecer(ctx context.Context, e interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}, driver, schema string) error {
	switch driver {
	case "mysql":
		quoted := "`" + strings.ReplaceAll(schema, "`", "``") + "`"
		if _, err := e.ExecContext(ctx, "USE "+quoted); err != nil {
			return fmt.Errorf("USE %s: %w", quoted, err)
		}
	case "postgres":
		quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`
		if _, err := e.ExecContext(ctx, "SET search_path TO "+quoted); err != nil {
			return fmt.Errorf("SET search_path TO %s: %w", quoted, err)
		}
	default:
		// Unknown driver — skip silently. The query may still work if the
		// SQL fully qualifies its table names.
	}
	return nil
}

type resolveErr struct{ msg string }

func (e *resolveErr) Error() string { return e.msg }

func stringArgsToAny(in []string) []interface{} {
	out := make([]interface{}, len(in))
	for i, a := range in {
		out[i] = a
	}
	return out
}

func buildColumnMetadata(cols []string, cts []*sql.ColumnType) []*pb.ColumnMetadata {
	out := make([]*pb.ColumnMetadata, len(cols))
	for i, c := range cols {
		var dbTypeName, scanHint string
		if i < len(cts) {
			dbTypeName = cts[i].DatabaseTypeName()
			if st := cts[i].ScanType(); st != nil {
				scanHint = scanTypeHint(st)
			}
		}
		out[i] = &pb.ColumnMetadata{
			Name:             c,
			DatabaseTypeName: dbTypeName,
			ScanTypeHint:     scanHint,
		}
	}
	return out
}

// scanTypeHint produces the over-the-wire hint string for a column's Go scan
// type. Plain Kind().String() works for the trivial cases (int64 / float64 /
// string / bool / slice) but is useless for struct-shaped types like time.Time
// or sql.NullXxx — they all collapse to "struct" and the cm side can't tell
// them apart. We special-case the common shapes.
func scanTypeHint(t reflect.Type) string {
	switch t {
	case reflect.TypeOf(time.Time{}):
		return "time"
	case reflect.TypeOf(sql.NullTime{}):
		return "null_time"
	case reflect.TypeOf(sql.NullString{}):
		return "null_string"
	case reflect.TypeOf(sql.NullInt64{}):
		return "null_int64"
	case reflect.TypeOf(sql.NullFloat64{}):
		return "null_float64"
	case reflect.TypeOf(sql.NullBool{}):
		return "null_bool"
	}
	return t.Kind().String()
}

// encodeValue turns a Go value (whatever the SQL driver scanned) into a
// wire-shaped *pb.Value. SQL NULL → empty Value (no oneof case set, sentinel
// is_null bit reused as the "explicit null" marker so the cloud can distinguish
// "no field set" from "actually null"). Driver-specific shapes:
//
//   - MySQL: strings come back as []byte, time as time.Time (with parseTime),
//     bool as int64, decimal as []byte.
//   - Postgres (lib/pq): strings come back as string, time as time.Time, bool
//     as bool, numeric as []byte.
//
// We collapse []byte → string when it's a valid string column — the cloud
// can't recover bytes vs string after the fact, but for almost every nuzur
// use case (data manager display, sql-to-json) string is what's wanted.
func encodeValue(v interface{}) *pb.Value {
	if v == nil {
		return &pb.Value{Kind: &pb.Value_IsNull{IsNull: true}}
	}
	switch x := v.(type) {
	case string:
		return &pb.Value{Kind: &pb.Value_StringVal{StringVal: x}}
	case []byte:
		return &pb.Value{Kind: &pb.Value_StringVal{StringVal: string(x)}}
	case int64:
		return &pb.Value{Kind: &pb.Value_IntVal{IntVal: x}}
	case int32:
		return &pb.Value{Kind: &pb.Value_IntVal{IntVal: int64(x)}}
	case int:
		return &pb.Value{Kind: &pb.Value_IntVal{IntVal: int64(x)}}
	case float64:
		return &pb.Value{Kind: &pb.Value_DoubleVal{DoubleVal: x}}
	case float32:
		return &pb.Value{Kind: &pb.Value_DoubleVal{DoubleVal: float64(x)}}
	case bool:
		return &pb.Value{Kind: &pb.Value_BoolVal{BoolVal: x}}
	case time.Time:
		return &pb.Value{Kind: &pb.Value_TimeVal{TimeVal: timestamppb.New(x)}}
	default:
		// Fallback for shapes we haven't enumerated (uuid.UUID, decimal, etc.):
		// stringify so the value is at least readable in the data manager.
		return &pb.Value{Kind: &pb.Value_StringVal{StringVal: fallbackStringify(x)}}
	}
}

func fallbackStringify(v interface{}) string {
	switch x := v.(type) {
	case fmt.Stringer:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}
