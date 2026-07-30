// Package sqlplan turns the apply SQL of a nuzur schema push into something a
// human can decide on: an ordered statement list, each labelled with what it does
// and what it can cost.
//
// It exists because the CLI receives the migration as one opaque string. sql-push
// hands over the exact DDL it is about to run — see the confirmation step in
// extension-sql-push's manager — and nothing between there and here says which of
// those statements deletes a table.
//
// # What this package cannot know
//
// Read this before trusting its output, and before extending it. A preview users
// over-trust is worse than no preview.
//
//  1. No per-statement hazards. pg-schema-diff computes Statement.Hazards, Timeout
//     and LockTimeout for every statement it generates, and every one of those is
//     discarded before the CLI sees it (in the local agent's pg-schema-plan
//     handler, twice in nuzur-go's sql-diff-manager, and finally by the wire types
//     themselves — ComputePgSchemaPlanResponse.apply_sql and
//     GetChangesDiffResponse.apply are plain strings). So this package cannot tell
//     you which statements take an ACCESS EXCLUSIVE lock and therefore take the
//     table offline, which are multi-hour index builds, or which the differ itself
//     considered correctness-affecting. It re-derives only what a keyword can see.
//  2. No row counts. "DROP TABLE audit_log_2023" cannot be annotated with how many
//     rows that is; nothing in the path counts.
//  3. No idea whether an ALTER will succeed. Adding a foreign key against orphan
//     rows, SET NOT NULL against nulls, a unique index against duplicates: all of
//     those are flagged "may fail", never resolved. pg-schema-diff could resolve
//     them by validating against a temporary database, but nuzur asks it not to.
//  4. No atomicity to report. sql-push does not ask for a transaction and the
//     query manager splits on ";" and runs the fragments one at a time, so a
//     failure at statement 7 of 12 leaves 1 through 6 applied. This package says
//     so; it cannot fix it.
//  5. MySQL diffs contain churn this package cannot identify. nuzur cannot read a
//     MySQL schema directly, so the "existing" side is reconstructed by
//     introspecting the database into a project version and re-rendering it as
//     DDL. Anything the model cannot express comes back normalized, producing
//     ALTERs that change nothing and reappear on every deploy. Those land in the
//     narrowing class; a caller with a MySQL target should say so out loud.
//
// The one thing it is careful to get right is the data-loss class, because that is
// what a caller gates on. Everything unrecognized is KindOther at SeverityNone:
// never read "not flagged" as "proven safe".
package sqlplan

import (
	"strings"
)

// Severity is what a statement can cost you.
type Severity string

const (
	// SeverityNone is additive: it creates or widens, and cannot fail against data
	// that is already there.
	SeverityNone Severity = ""
	// SeverityDataLoss means rows or column values disappear. This is the class
	// callers gate on.
	SeverityDataLoss Severity = "data_loss"
	// SeverityConstraintLoss means an index, key or constraint disappears. No data
	// is deleted, but a guarantee — and possibly a query plan — is.
	SeverityConstraintLoss Severity = "constraint_loss"
	// SeverityNarrowing means the statement may fail, or truncate, when applied to
	// rows that already exist.
	SeverityNarrowing Severity = "narrowing"
)

// rank orders severities so the worst action in a multi-action ALTER decides the
// statement. Only the position of SeverityDataLoss at the top is load-bearing.
func rank(s Severity) int {
	switch s {
	case SeverityDataLoss:
		return 3
	case SeverityConstraintLoss:
		return 2
	case SeverityNarrowing:
		return 1
	default:
		return 0
	}
}

// Kind is what a statement does, at the granularity a reader cares about.
type Kind string

const (
	KindCreateTable    Kind = "create_table"
	KindCreateIndex    Kind = "create_index"
	KindCreateSchema   Kind = "create_schema"
	KindAddColumn      Kind = "add_column"
	KindAddConstraint  Kind = "add_constraint"
	KindAlterColumn    Kind = "alter_column"
	KindDropTable      Kind = "drop_table"
	KindDropColumn     Kind = "drop_column"
	KindDropIndex      Kind = "drop_index"
	KindDropConstraint Kind = "drop_constraint"
	KindDropSchema     Kind = "drop_schema"
	KindDropDatabase   Kind = "drop_database"
	KindTruncate       Kind = "truncate"
	KindOther          Kind = "other"
)

// Statement is one fragment of the migration, as it will be executed.
type Statement struct {
	// Index is 1-based and is execution order.
	Index int `json:"index"`
	// SQL is the statement as it will be sent, without its trailing ";".
	SQL      string   `json:"sql"`
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity,omitempty"`
	// Object is a best-effort "table" or "table.column" — for pointing at the
	// thing being lost, not for programmatic use.
	Object string `json:"object,omitempty"`
	// Reason says what this costs, in a sentence a person can act on.
	Reason string `json:"reason,omitempty"`
}

// Destructive reports whether this statement deletes data.
func (s Statement) Destructive() bool { return s.Severity == SeverityDataLoss }

// Plan is an ordered migration.
type Plan struct {
	Statements []Statement `json:"statements"`
}

// Counts summarizes a plan by severity.
type Counts struct {
	Total          int `json:"total"`
	Additive       int `json:"additive"`
	DataLoss       int `json:"data_loss"`
	ConstraintLoss int `json:"constraint_loss"`
	Narrowing      int `json:"narrowing"`
}

// Counts summarizes the plan by severity.
func (p Plan) Counts() Counts {
	c := Counts{Total: len(p.Statements)}
	for _, s := range p.Statements {
		switch s.Severity {
		case SeverityDataLoss:
			c.DataLoss++
		case SeverityConstraintLoss:
			c.ConstraintLoss++
		case SeverityNarrowing:
			c.Narrowing++
		default:
			c.Additive++
		}
	}
	return c
}

// Empty reports whether there is nothing to apply.
func (p Plan) Empty() bool { return len(p.Statements) == 0 }

// HasDestructive reports whether any statement deletes data. This is the question
// the deploy gate asks.
func (p Plan) HasDestructive() bool { return len(p.Destructive()) > 0 }

// Destructive returns the statements that delete data, in execution order.
func (p Plan) Destructive() []Statement {
	var out []Statement
	for _, s := range p.Statements {
		if s.Destructive() {
			out = append(out, s)
		}
	}
	return out
}

// Analyze splits apply SQL into statements and labels each one.
func Analyze(applySQL string) Plan {
	frags := Split(applySQL)
	p := Plan{Statements: make([]Statement, 0, len(frags))}
	for idx, frag := range frags {
		kind, sev, object, reason := classify(frag)
		p.Statements = append(p.Statements, Statement{
			Index:    idx + 1,
			SQL:      frag,
			Kind:     kind,
			Severity: sev,
			Object:   object,
			Reason:   reason,
		})
	}
	return p
}

// Split splits apply SQL into statements exactly the way the connection manager
// will execute it: on ";", discarding fragments that are only whitespace.
//
// This is deliberately the same naive split as executeRawQuery in nuzur-go's
// sql-query-manager. A smarter splitter would produce a prettier preview and a
// less truthful one: whatever a ";" inside a string literal does to the real
// execution, the preview has to show the same thing. If executeRawQuery ever
// learns to parse, this must learn with it.
//
// Fragments are trimmed for display. Leading and trailing whitespace is the one
// difference from what is executed, and it changes nothing about what runs.
func Split(applySQL string) []string {
	var out []string
	for _, frag := range strings.Split(applySQL, ";") {
		if t := strings.TrimSpace(frag); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// classify labels one statement lexically — there is no SQL parser here.
//
// The input is machine-generated DDL from two known generators (pg-schema-diff and
// planetscale/schemadiff), not arbitrary user SQL, and on that input matching the
// leading keywords gets the same answers a parser would for none of the
// dependency. Dispatching on the LEADING verb rather than searching the whole
// string is what keeps `CREATE TABLE "drop_log"` additive.
func classify(sql string) (Kind, Severity, string, string) {
	norm := normalize(sql)
	switch {
	case hasPrefixWord(norm, "CREATE TABLE"):
		return KindCreateTable, SeverityNone, objectAfter(norm, "CREATE TABLE"), ""
	case hasPrefixWord(norm, "CREATE UNIQUE INDEX"):
		obj := objectAfter(norm, "CREATE UNIQUE INDEX")
		return KindCreateIndex, SeverityNarrowing, obj, "adds a uniqueness rule; fails if the existing rows already contain duplicates"
	case hasPrefixWord(norm, "CREATE INDEX"):
		return KindCreateIndex, SeverityNone, objectAfter(norm, "CREATE INDEX"), ""
	case hasPrefixWord(norm, "CREATE SCHEMA"):
		return KindCreateSchema, SeverityNone, objectAfter(norm, "CREATE SCHEMA"), ""
	case hasPrefixWord(norm, "DROP TABLE"):
		obj := objectAfter(norm, "DROP TABLE")
		return KindDropTable, SeverityDataLoss, obj, "drops " + describe(obj, "the table") + " and every row in it"
	case hasPrefixWord(norm, "DROP DATABASE"):
		obj := objectAfter(norm, "DROP DATABASE")
		return KindDropDatabase, SeverityDataLoss, obj, "drops " + describe(obj, "the database") + " and everything in it"
	case hasPrefixWord(norm, "DROP SCHEMA"):
		obj := objectAfter(norm, "DROP SCHEMA")
		// The differ attaches no hazard to this one, which is why the classifier
		// cannot be replaced by hazards alone even after they reach the CLI.
		return KindDropSchema, SeverityDataLoss, obj, "drops " + describe(obj, "the schema") + " and every table in it"
	case hasPrefixWord(norm, "TRUNCATE"):
		obj := objectAfter(norm, "TRUNCATE TABLE")
		if obj == "" {
			obj = objectAfter(norm, "TRUNCATE")
		}
		return KindTruncate, SeverityDataLoss, obj, "deletes every row in " + describe(obj, "the table")
	case hasPrefixWord(norm, "DROP INDEX"):
		obj := objectAfter(norm, "DROP INDEX")
		return KindDropIndex, SeverityConstraintLoss, obj, "drops " + describe(obj, "the index") + "; no data is deleted, but queries relying on it get slower and any uniqueness it enforced is gone"
	case hasPrefixWord(norm, "ALTER TABLE"):
		return classifyAlterTable(norm)
	default:
		return KindOther, SeverityNone, "", ""
	}
}

// classifyAlterTable labels an ALTER TABLE by scanning its action list.
//
// Both generators emit multi-action ALTERs (`ALTER TABLE t ADD COLUMN a INT, DROP
// COLUMN b`), so the statement takes the WORST severity among its actions and the
// kind of whichever action that was. Anything else would let a data-loss action
// hide behind an additive one in the same statement.
func classifyAlterTable(norm string) (Kind, Severity, string, string) {
	table := objectAfter(norm, "ALTER TABLE")
	rest := strings.TrimSpace(strings.TrimPrefix(norm, "ALTER TABLE"))
	// Step past the table name to the action list.
	if idx := strings.IndexFunc(rest, isSpace); idx >= 0 {
		rest = rest[idx:]
	} else {
		rest = ""
	}

	worstKind, worstSev, worstObj, worstReason := KindOther, SeverityNone, table, ""
	for _, action := range splitTopLevel(rest, ',') {
		kind, sev, col, reason := classifyAlterAction(strings.TrimSpace(action))
		if rank(sev) > rank(worstSev) || (worstKind == KindOther && kind != KindOther) {
			worstKind, worstSev, worstReason = kind, sev, reason
			worstObj = table
			if col != "" {
				worstObj = joinObject(table, col)
			}
		}
	}
	if worstReason != "" {
		worstReason = strings.ReplaceAll(worstReason, "{table}", describe(table, "the table"))
	}
	return worstKind, worstSev, worstObj, worstReason
}

// classifyAlterAction labels a single ALTER TABLE action. The returned object is
// the column or constraint name, if one is identifiable.
func classifyAlterAction(a string) (Kind, Severity, string, string) {
	switch {
	// DROP COLUMN, in both spellings. MySQL's bare `DROP col` means DROP COLUMN,
	// so it has to be matched — but only after every other DROP form has been
	// ruled out, or dropping a key would read as dropping a column.
	case hasPrefixWord(a, "DROP COLUMN"):
		col := objectAfter(a, "DROP COLUMN")
		return KindDropColumn, SeverityDataLoss, col, "drops " + describe(col, "the column") + " from {table} and every value in it"
	case hasPrefixWord(a, "DROP CONSTRAINT"):
		return KindDropConstraint, SeverityConstraintLoss, objectAfter(a, "DROP CONSTRAINT"), "drops a constraint on {table}"
	case hasPrefixWord(a, "DROP FOREIGN KEY"):
		return KindDropConstraint, SeverityConstraintLoss, objectAfter(a, "DROP FOREIGN KEY"), "drops a foreign key on {table}"
	case hasPrefixWord(a, "DROP CHECK"):
		return KindDropConstraint, SeverityConstraintLoss, objectAfter(a, "DROP CHECK"), "drops a check constraint on {table}"
	case hasPrefixWord(a, "DROP PRIMARY KEY"):
		return KindDropIndex, SeverityConstraintLoss, "", "drops the primary key of {table}"
	case hasPrefixWord(a, "DROP KEY"):
		return KindDropIndex, SeverityConstraintLoss, objectAfter(a, "DROP KEY"), "drops an index on {table}"
	case hasPrefixWord(a, "DROP INDEX"):
		return KindDropIndex, SeverityConstraintLoss, objectAfter(a, "DROP INDEX"), "drops an index on {table}"
	case hasPrefixWord(a, "DROP"):
		// MySQL's bare form. Everything above has been excluded, so what remains
		// is a column name.
		col := objectAfter(a, "DROP")
		return KindDropColumn, SeverityDataLoss, col, "drops " + describe(col, "the column") + " from {table} and every value in it"

	case hasPrefixWord(a, "ADD COLUMN"):
		return KindAddColumn, SeverityNone, objectAfter(a, "ADD COLUMN"), ""
	case hasPrefixWord(a, "ADD UNIQUE"):
		// Covers MySQL's ADD UNIQUE INDEX / ADD UNIQUE KEY as well.
		return KindAddConstraint, SeverityNarrowing, "", "adds a uniqueness rule to {table}; fails if the existing rows already contain duplicates"
	case hasPrefixWord(a, "ADD CONSTRAINT"), hasPrefixWord(a, "ADD PRIMARY KEY"),
		hasPrefixWord(a, "ADD FOREIGN KEY"), hasPrefixWord(a, "ADD CHECK"):
		// A constraint added to a table that already has rows fails if any of them
		// violate it. Which rows those are is exactly what this package cannot
		// know, so it says "may fail" and stops.
		return KindAddConstraint, SeverityNarrowing, "", "adds a constraint to {table}; fails if the existing rows do not already satisfy it"
	case hasPrefixWord(a, "ADD INDEX"), hasPrefixWord(a, "ADD KEY"):
		return KindCreateIndex, SeverityNone, "", ""

	// Column redefinitions. On MySQL these are also where the reconstructed
	// "existing" side produces churn that changes nothing.
	case hasPrefixWord(a, "MODIFY COLUMN"), hasPrefixWord(a, "MODIFY"),
		hasPrefixWord(a, "CHANGE COLUMN"), hasPrefixWord(a, "CHANGE"):
		return KindAlterColumn, SeverityNarrowing, "", "redefines a column on {table}; can truncate or fail depending on the values already stored"
	case hasPrefixWord(a, "ALTER COLUMN"):
		switch {
		case containsWord(a, "SET NOT NULL"):
			return KindAlterColumn, SeverityNarrowing, objectAfter(a, "ALTER COLUMN"), "requires every existing row of {table} to have a value in this column; fails on nulls"
		case containsWord(a, "TYPE"):
			return KindAlterColumn, SeverityNarrowing, objectAfter(a, "ALTER COLUMN"), "changes a column type on {table}; can truncate or fail depending on the values already stored"
		default:
			// DROP NOT NULL, SET/DROP DEFAULT: widening or metadata-only.
			return KindAlterColumn, SeverityNone, objectAfter(a, "ALTER COLUMN"), ""
		}
	default:
		return KindOther, SeverityNone, "", ""
	}
}

// normalize produces the uppercased, comment-free, single-spaced form used for
// matching. The original string is what gets displayed and executed; this copy
// exists only to be pattern-matched against.
func normalize(sql string) string {
	return collapseSpace(strings.ToUpper(stripComments(sql)))
}

// stripComments removes -- line comments and /* */ block comments, leaving a space
// where each was so tokens on either side do not merge.
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		switch {
		case s[i] == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case s[i] == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < len(s) {
				i += 2
			} else {
				i = len(s)
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// collapseSpace turns every run of whitespace into a single space and trims. It is
// what lets a pattern like "DROP COLUMN" match DDL that wrapped across lines.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// hasPrefixWord reports whether norm starts with the given keyword phrase at a
// word boundary. `hasPrefixWord("DROPPED_AT ...", "DROP")` is false.
func hasPrefixWord(norm, phrase string) bool {
	if !strings.HasPrefix(norm, phrase) {
		return false
	}
	rest := norm[len(phrase):]
	return rest == "" || rest[0] == ' ' || rest[0] == '(' || rest[0] == '"' || rest[0] == '`'
}

// containsWord reports whether the phrase appears delimited by spaces or ends the
// string — used for mid-statement clauses like SET NOT NULL.
func containsWord(norm, phrase string) bool {
	idx := strings.Index(norm, phrase)
	for idx >= 0 {
		beforeOK := idx == 0 || norm[idx-1] == ' '
		after := idx + len(phrase)
		afterOK := after == len(norm) || norm[after] == ' '
		if beforeOK && afterOK {
			return true
		}
		next := strings.Index(norm[idx+1:], phrase)
		if next < 0 {
			return false
		}
		idx = idx + 1 + next
	}
	return false
}

// objectAfter returns the identifier following a keyword phrase, unquoted, or "".
// Best-effort by design: it labels output, and no decision depends on it.
func objectAfter(norm, phrase string) string {
	idx := strings.Index(norm, phrase)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(norm[idx+len(phrase):])
	rest = strings.TrimPrefix(rest, "IF EXISTS ")
	rest = strings.TrimPrefix(rest, "IF NOT EXISTS ")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	// Take up to the first delimiter that cannot be part of a qualified name.
	end := strings.IndexAny(rest, " (,")
	if end < 0 {
		end = len(rest)
	}
	return unquoteIdent(rest[:end])
}

// unquoteIdent strips the quoting styles the generators emit, per dotted part, so
// `"public"."orders"` reads as public.orders.
func unquoteIdent(id string) string {
	parts := strings.Split(id, ".")
	for i, p := range parts {
		p = strings.Trim(p, "\"`[]")
		parts[i] = strings.ToLower(p)
	}
	return strings.Join(parts, ".")
}

// joinObject qualifies a column with its table, skipping either if absent.
func joinObject(table, col string) string {
	switch {
	case table == "":
		return col
	case col == "":
		return table
	default:
		return table + "." + col
	}
}

// describe renders an object for a sentence, falling back to a generic noun when
// the identifier could not be extracted.
func describe(object, fallback string) string {
	if object == "" {
		return fallback
	}
	return object
}

// splitTopLevel splits on a separator that is not nested inside parentheses or
// quotes — needed because an ALTER's action list is comma-separated while its
// column definitions contain commas of their own, as in DECIMAL(10,2).
func splitTopLevel(s string, sep rune) []string {
	var (
		out   []string
		cur   strings.Builder
		depth int
		quote rune
	)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"' || r == '`':
			quote = r
			cur.WriteRune(r)
		case r == '(':
			depth++
			cur.WriteRune(r)
		case r == ')':
			depth--
			cur.WriteRune(r)
		case r == sep && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}
