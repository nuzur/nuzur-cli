package sqlplan

import (
	"fmt"
	"strings"
)

// Rendering lives here as pure string building; deciding what to print, and in
// which color, belongs to the caller.
//
// The annotation format deliberately mirrors pg-schema-diff's own plan output
// ("-- HAZARD <TYPE>: <message>"), because that is the tool actually generating
// these statements for Postgres and somebody who has read one of its plans should
// recognize ours. The hazard names are its names.
const (
	hazardDeletesData  = "DELETES_DATA"
	hazardIndexDropped = "INDEX_DROPPED"
	hazardCorrectness  = "CORRECTNESS"
)

// hazardName maps a severity onto the vocabulary pg-schema-diff uses.
func hazardName(s Severity) string {
	switch s {
	case SeverityDataLoss:
		return hazardDeletesData
	case SeverityConstraintLoss:
		return hazardIndexDropped
	case SeverityNarrowing:
		return hazardCorrectness
	default:
		return ""
	}
}

// SummaryLine is the one-line count of what this plan does.
func (p Plan) SummaryLine() string {
	c := p.Counts()
	if c.Total == 0 {
		return "No changes — the database already matches the model."
	}
	parts := []string{fmt.Sprintf("%d additive", c.Additive)}
	if c.DataLoss > 0 {
		parts = append(parts, fmt.Sprintf("%d DESTRUCTIVE (delete data)", c.DataLoss))
	}
	if c.ConstraintLoss > 0 {
		parts = append(parts, fmt.Sprintf("%d index/constraint drop%s", c.ConstraintLoss, suffix(c.ConstraintLoss)))
	}
	if c.Narrowing > 0 {
		parts = append(parts, fmt.Sprintf("%d that may fail on existing rows", c.Narrowing))
	}
	return fmt.Sprintf("%s: %s", plural(c.Total, "statement"), strings.Join(parts, ", "))
}

// RenderDestructive is the called-out block of statements that delete data, or ""
// when there are none. Numbering is the plan's execution order, so a reader can
// find each one in the full listing.
func (p Plan) RenderDestructive() string {
	dest := p.Destructive()
	if len(dest) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("DESTRUCTIVE — these delete data:\n")
	for _, s := range dest {
		writeStatement(&b, s)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderStatements is the full plan in execution order, each statement annotated
// with what it costs.
func (p Plan) RenderStatements() string {
	if len(p.Statements) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range p.Statements {
		writeStatement(&b, s)
	}
	return strings.TrimRight(b.String(), "\n")
}

// TransactionalWarning says what a partial failure costs on this engine, for this
// plan. Empty for a plan of fewer than two statements, where "the ones before it
// stayed applied" describes nothing.
//
// It used to be one unconditional sentence — "these run ONE AT A TIME, not in a
// transaction" — which was true when sql-push did not request a transaction. It now
// does, and the answer became engine- and content-dependent, so a single sentence can
// no longer be honest for every plan. Getting this wrong in the reassuring direction
// is the expensive one: a reader who believes a failed migration rolled back will not
// go and check what actually landed.
func (p Plan) TransactionalWarning(engine Engine) string {
	if len(p.Statements) < 2 {
		return ""
	}
	switch engine {
	case EnginePostgres:
		if forced := p.nonTransactionalStatements(); len(forced) > 0 {
			return fmt.Sprintf(
				"These statements do NOT run in a transaction. %s cannot run inside one, and\n"+
					"Postgres allows no partial exception, so the WHOLE migration is applied one\n"+
					"statement at a time: if one fails partway through, the statements before it stay\n"+
					"applied.", describeStatements(forced))
		}
		return "These statements are applied in a TRANSACTION: if one fails, the whole migration\n" +
			"rolls back and the database is left exactly as it was. That is not a promise the\n" +
			"migration will succeed — only that a failure will not leave it half-applied."
	case EngineMySQL:
		return "These statements effectively commit ONE AT A TIME. A transaction is opened, but\n" +
			"MySQL commits DDL implicitly, so it cannot be rolled back: if one statement fails\n" +
			"partway through, the statements before it stay applied."
	default:
		// Engine unknown: say the thing that is true on every engine rather than the
		// one that would be reassuring on some of them.
		return "Do not assume this migration is atomic: depending on the engine and on what it\n" +
			"contains, a statement that fails partway through can leave the statements before\n" +
			"it applied. Check the database's state before retrying."
	}
}

// Transactional reports whether the whole plan is applied as one unit — true only on
// Postgres, and only when nothing in it forces the executor off the transactional
// path.
func (p Plan) Transactional(engine Engine) bool {
	return engine == EnginePostgres && len(p.nonTransactionalStatements()) == 0
}

// describeStatements names the offending statements by execution index.
func describeStatements(idx []int) string {
	strs := make([]string, 0, len(idx))
	for _, i := range idx {
		strs = append(strs, fmt.Sprintf("%d", i))
	}
	if len(strs) == 1 {
		return "Statement " + strs[0]
	}
	return "Statements " + strings.Join(strs[:len(strs)-1], ", ") + " and " + strs[len(strs)-1]
}

// MySQLCaveat explains why a MySQL plan can contain statements that change
// nothing. Callers print it whenever the target is MySQL.
//
// It matters because MySQL is the default engine, and because a user who sees a
// dozen pointless MODIFY COLUMNs and is not told why will conclude the tool is
// broken — which is roughly what happened to the user this feature was built for.
func MySQLCaveat() string {
	return "MySQL note: nuzur cannot read a MySQL schema directly, so the \"existing\" side of\n" +
		"this diff is reconstructed — the database is introspected into a project version and\n" +
		"re-rendered as DDL. Anything the model cannot express comes back normalized, so this\n" +
		"plan can contain ALTERs that change nothing and that will reappear on every deploy\n" +
		"(column widths and types are the usual ones). The CREATEs and DROPs are real.\n" +
		"Treat a bare MODIFY/CHANGE COLUMN as suspect and compare it against the column you\n" +
		"actually have."
}

// ChurnNote reports how much of a MySQL plan is likely to be that no-op churn, or
// "" when none of it is.
//
// It used to count column redefinitions only, which missed the other shape entirely
// and the largest single source of a plan that never converges: when the
// reconstruction loses an index's TYPE, the differ sees a type change and proposes to
// drop the index and add it back — every run, forever. Those land as an index drop
// plus an index create, neither of which is a column redefinition, so a plan that was
// pure churn from top to bottom got no note at all.
func (p Plan) ChurnNote() string {
	c := p.Counts()
	redefines, indexes := 0, 0
	for _, s := range p.Statements {
		switch {
		case s.Kind == KindAlterColumn && s.Severity == SeverityNarrowing:
			redefines++
		case s.Kind == KindDropIndex || s.Kind == KindCreateIndex:
			indexes++
		}
	}
	churn := redefines + indexes
	if churn == 0 {
		return ""
	}
	var parts []string
	if redefines > 0 {
		parts = append(parts, fmt.Sprintf("%d redefine a column", redefines))
	}
	if indexes > 0 {
		parts = append(parts, fmt.Sprintf("%d drop or add an index", indexes))
	}
	return fmt.Sprintf(
		"%d of %d statements %s — on MySQL those are the two shapes no-op churn takes, and\n"+
			"a statement that reappears on every deploy is almost certainly one of them.\n"+
			"Compare them against the schema you actually have before reading them as changes.",
		churn, c.Total, strings.Join(parts, " and "))
}

// DropOnlyWhatItCouldCreate is the bound on a reconciling deploy's blast radius,
// and no user currently knows it.
//
// The diff restricts BOTH sides to what a project version can express — on
// Postgres by an object-class allowlist (schemas, tables, indexes, foreign keys),
// on MySQL because both sides are rendered by the same generator. Anything the
// model cannot represent is therefore absent from both sides and can never be
// proposed for a drop.
func DropOnlyWhatItCouldCreate() string {
	return "Only tables, columns, indexes, foreign keys and schemas are reconciled. Anything\n" +
		"the nuzur model cannot express — triggers, functions, views, sequences, row-level\n" +
		"security, check constraints — is invisible to this diff and is never dropped by it.\n" +
		"Column defaults and foreign key referential actions ARE part of the model and are\n" +
		"reconciled: a default or an ON DELETE/ON UPDATE the model does not state is dropped\n" +
		"from the database like any other column change."
}

// writeStatement renders one numbered statement plus its hazard annotation.
func writeStatement(b *strings.Builder, s Statement) {
	fmt.Fprintf(b, "%4d. %s\n", s.Index, s.SQL)
	if name := hazardName(s.Severity); name != "" {
		reason := s.Reason
		if reason == "" {
			reason = string(s.Kind)
		}
		fmt.Fprintf(b, "      -- HAZARD %s: %s\n", name, reason)
	}
}

func plural(n int, noun string) string {
	return fmt.Sprintf("%d %s%s", n, noun, suffix(n))
}

func suffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
