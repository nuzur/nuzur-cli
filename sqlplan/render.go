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
	hazardDeletesData    = "DELETES_DATA"
	hazardIndexDropped   = "INDEX_DROPPED"
	hazardCorrectness    = "CORRECTNESS"
	transactionalWarning = "These statements run ONE AT A TIME, not in a transaction: if one fails partway\nthrough, the statements before it stay applied."
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

// TransactionalWarning is the note that a partial failure leaves a partly-migrated
// database. It is unconditional for a non-empty plan: sql-push never asks for a
// transaction, so this is always true and has never been said out loud.
func (p Plan) TransactionalWarning() string {
	if len(p.Statements) < 2 {
		return ""
	}
	return transactionalWarning
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
func (p Plan) ChurnNote() string {
	c := p.Counts()
	churn := 0
	for _, s := range p.Statements {
		if s.Kind == KindAlterColumn && s.Severity == SeverityNarrowing {
			churn++
		}
	}
	if churn == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d statements redefine a column — on MySQL that is usually no-op churn, not a real change.", churn, c.Total)
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
		"security, check constraints, column defaults — is invisible to this diff and is\n" +
		"never dropped by it."
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
