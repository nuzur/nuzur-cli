package sqlplan

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "whitespace only", in: "  \n\t ", want: nil},
		{name: "single with trailing semicolon", in: "CREATE TABLE a (id INT);", want: []string{"CREATE TABLE a (id INT)"}},
		{name: "single without trailing semicolon", in: "CREATE TABLE a (id INT)", want: []string{"CREATE TABLE a (id INT)"}},
		{
			name: "pg-schema-diff style, semicolon plus newline",
			in:   "CREATE TABLE a (id INT);\nDROP TABLE b;\n",
			want: []string{"CREATE TABLE a (id INT)", "DROP TABLE b"},
		},
		{name: "blank fragments between statements are dropped", in: "A;;;B;", want: []string{"A", "B"}},
		{
			// This is not a bug being tolerated, it is the executor's behavior being
			// reproduced: nuzur-go's executeRawQuery splits on ";" too, so a
			// semicolon inside a literal breaks the real execution exactly here. A
			// preview that hid the split would be lying about what runs.
			name: "semicolon inside a literal splits, matching the executor",
			in:   "INSERT INTO t VALUES ('a;b')",
			want: []string{"INSERT INTO t VALUES ('a", "b')"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Split(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("Split(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Split(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sql      string
		wantKind Kind
		wantSev  Severity
		wantObj  string // "" == don't check
	}{
		// ---- additive ----
		{name: "create table", sql: `CREATE TABLE "public"."leads" (id UUID PRIMARY KEY)`, wantKind: KindCreateTable, wantSev: SeverityNone, wantObj: "public.leads"},
		{name: "create index", sql: "CREATE INDEX idx_leads_email ON leads (email)", wantKind: KindCreateIndex, wantSev: SeverityNone},
		{name: "create schema", sql: "CREATE SCHEMA crm", wantKind: KindCreateSchema, wantSev: SeverityNone, wantObj: "crm"},
		{name: "add column", sql: "ALTER TABLE leads ADD COLUMN tz VARCHAR(64)", wantKind: KindAddColumn, wantSev: SeverityNone},
		{name: "mysql add index", sql: "ALTER TABLE `leads` ADD INDEX `idx_tz` (`tz`)", wantKind: KindCreateIndex, wantSev: SeverityNone},
		{name: "drop not null is widening", sql: `ALTER TABLE leads ALTER COLUMN email DROP NOT NULL`, wantKind: KindAlterColumn, wantSev: SeverityNone},
		{name: "set default is metadata only", sql: `ALTER TABLE leads ALTER COLUMN tz SET DEFAULT 'UTC'`, wantKind: KindAlterColumn, wantSev: SeverityNone},

		// ---- data loss ----
		{name: "drop table", sql: `DROP TABLE "public"."audit_log_2023"`, wantKind: KindDropTable, wantSev: SeverityDataLoss, wantObj: "public.audit_log_2023"},
		{name: "drop table if exists", sql: "DROP TABLE IF EXISTS clients", wantKind: KindDropTable, wantSev: SeverityDataLoss, wantObj: "clients"},
		{name: "drop database", sql: "DROP DATABASE acme", wantKind: KindDropDatabase, wantSev: SeverityDataLoss, wantObj: "acme"},
		{
			// The differ emits DROP SCHEMA with NO hazard metadata attached, so a
			// hazards-only classifier would wave through the single most
			// destructive statement it can produce. This case is why the lexical
			// classifier stays even after hazards reach the CLI.
			name: "drop schema", sql: "DROP SCHEMA crm", wantKind: KindDropSchema, wantSev: SeverityDataLoss, wantObj: "crm",
		},
		{name: "truncate", sql: "TRUNCATE TABLE leads", wantKind: KindTruncate, wantSev: SeverityDataLoss, wantObj: "leads"},
		{name: "truncate without TABLE", sql: "TRUNCATE leads", wantKind: KindTruncate, wantSev: SeverityDataLoss, wantObj: "leads"},
		{name: "drop column", sql: `ALTER TABLE "public"."orders" DROP COLUMN "legacy_ref"`, wantKind: KindDropColumn, wantSev: SeverityDataLoss, wantObj: "public.orders.legacy_ref"},
		{
			// MySQL's bare DROP means DROP COLUMN. Getting this wrong would let a
			// column drop through the gate on the default engine.
			name: "mysql bare DROP is a column drop", sql: "ALTER TABLE `leads` DROP `tz`", wantKind: KindDropColumn, wantSev: SeverityDataLoss, wantObj: "leads.tz",
		},

		// ---- constraint loss: not data loss, must not block a deploy ----
		{name: "drop index", sql: "DROP INDEX idx_leads_email", wantKind: KindDropIndex, wantSev: SeverityConstraintLoss, wantObj: "idx_leads_email"},
		{name: "mysql drop key", sql: "ALTER TABLE `leads` DROP KEY `uq_email`", wantKind: KindDropIndex, wantSev: SeverityConstraintLoss},
		{name: "drop primary key", sql: "ALTER TABLE leads DROP PRIMARY KEY", wantKind: KindDropIndex, wantSev: SeverityConstraintLoss},
		{name: "drop constraint", sql: "ALTER TABLE orders DROP CONSTRAINT fk_orders_lead", wantKind: KindDropConstraint, wantSev: SeverityConstraintLoss},
		{name: "mysql drop foreign key", sql: "ALTER TABLE orders DROP FOREIGN KEY fk_orders_lead", wantKind: KindDropConstraint, wantSev: SeverityConstraintLoss},

		// ---- narrowing: may fail against rows that already exist ----
		{name: "create unique index", sql: "CREATE UNIQUE INDEX uq_skey ON app_settings (skey)", wantKind: KindCreateIndex, wantSev: SeverityNarrowing},
		{name: "add unique constraint", sql: "ALTER TABLE app_settings ADD UNIQUE (skey)", wantKind: KindAddConstraint, wantSev: SeverityNarrowing},
		{name: "mysql add unique index", sql: "ALTER TABLE app_settings ADD UNIQUE INDEX uq_skey (skey)", wantKind: KindAddConstraint, wantSev: SeverityNarrowing},
		{name: "add foreign key", sql: "ALTER TABLE orders ADD CONSTRAINT fk_lead FOREIGN KEY (lead_uuid) REFERENCES leads (uuid)", wantKind: KindAddConstraint, wantSev: SeverityNarrowing},
		{name: "set not null", sql: "ALTER TABLE leads ALTER COLUMN email SET NOT NULL", wantKind: KindAlterColumn, wantSev: SeverityNarrowing},
		{name: "alter column type", sql: "ALTER TABLE leads ALTER COLUMN tz TYPE VARCHAR(128)", wantKind: KindAlterColumn, wantSev: SeverityNarrowing},
		{name: "mysql modify column", sql: "ALTER TABLE `leads` MODIFY COLUMN `tz` VARCHAR(512)", wantKind: KindAlterColumn, wantSev: SeverityNarrowing},
		{name: "mysql change column", sql: "ALTER TABLE `leads` CHANGE COLUMN `tz` `tz` VARCHAR(512)", wantKind: KindAlterColumn, wantSev: SeverityNarrowing},

		// ---- the traps ----
		{
			// The case a strings.Contains(sql, "DROP") classifier gets wrong. A table
			// whose NAME contains "drop" is still just a CREATE.
			name: "DROP inside an identifier is still additive", sql: `CREATE TABLE "drop_log" (id UUID PRIMARY KEY, dropped_at TIMESTAMP)`, wantKind: KindCreateTable, wantSev: SeverityNone, wantObj: "drop_log",
		},
		{
			name: "DROP inside a string literal is still additive", sql: `ALTER TABLE audit ALTER COLUMN kind SET DEFAULT 'DROP TABLE'`, wantKind: KindAlterColumn, wantSev: SeverityNone,
		},
		{
			// planetscale emits comma-joined actions. The worst one has to decide the
			// statement, or a column drop hides behind an additive sibling.
			name: "multi-action ALTER takes the worst severity", sql: "ALTER TABLE t ADD COLUMN a INT, DROP COLUMN b", wantKind: KindDropColumn, wantSev: SeverityDataLoss, wantObj: "t.b",
		},
		{
			name: "multi-action ALTER, destructive action first", sql: "ALTER TABLE t DROP COLUMN b, ADD COLUMN a INT", wantKind: KindDropColumn, wantSev: SeverityDataLoss, wantObj: "t.b",
		},
		{
			// A comma inside DECIMAL(10,2) must not be read as an action separator.
			name: "commas inside a type are not action separators", sql: "ALTER TABLE t ADD COLUMN amount DECIMAL(10,2) NOT NULL", wantKind: KindAddColumn, wantSev: SeverityNone,
		},
		{
			name: "lowercase is classified the same", sql: `drop table "public"."orders"`, wantKind: KindDropTable, wantSev: SeverityDataLoss, wantObj: "public.orders",
		},
		{
			name: "a leading comment does not hide the verb", sql: "-- rebuilding the table\nDROP TABLE orders", wantKind: KindDropTable, wantSev: SeverityDataLoss, wantObj: "orders",
		},
		{
			name: "a block comment does not hide the verb", sql: "/* generated */ DROP TABLE orders", wantKind: KindDropTable, wantSev: SeverityDataLoss, wantObj: "orders",
		},
		{
			name: "a statement wrapped across lines still matches", sql: "ALTER TABLE\n  orders\n  DROP COLUMN\n  legacy_ref", wantKind: KindDropColumn, wantSev: SeverityDataLoss, wantObj: "orders.legacy_ref",
		},
		{
			// Anything unrecognized is inert. Never read "not flagged" as "safe".
			name: "unrecognized statement is inert", sql: "GRANT SELECT ON leads TO app", wantKind: KindOther, wantSev: SeverityNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := classify(tc.sql)
			if c.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", c.Kind, tc.wantKind)
			}
			if c.Severity != tc.wantSev {
				t.Errorf("severity = %q, want %q", c.Severity, tc.wantSev)
			}
			if tc.wantObj != "" && c.Object != tc.wantObj {
				t.Errorf("object = %q, want %q", c.Object, tc.wantObj)
			}
			// Every flagged statement must explain itself: the annotation is the
			// whole point of flagging it.
			if c.Severity != SeverityNone && strings.TrimSpace(c.Reason) == "" {
				t.Errorf("severity %q carries no reason", c.Severity)
			}
			// The summary fields are the worst hazard, and a flagged statement always
			// carries at least the one it was flagged for.
			switch {
			case c.Severity == SeverityNone && len(c.Hazards) != 0:
				t.Errorf("an inert statement carries hazards: %+v", c.Hazards)
			case c.Severity != SeverityNone && len(c.Hazards) == 0:
				t.Errorf("severity %q carries no hazard entry", c.Severity)
			case c.Severity != SeverityNone && c.Hazards[0].Severity != c.Severity:
				t.Errorf("hazards are not worst-first: summary %q, first hazard %q", c.Severity, c.Hazards[0].Severity)
			}
			// A reason must never leak the internal {table} placeholder.
			for _, h := range append([]Hazard{{Reason: c.Reason}}, c.Hazards...) {
				if strings.Contains(h.Reason, "{table}") {
					t.Errorf("reason leaked a placeholder: %q", h.Reason)
				}
			}
		})
	}
}

func TestAnalyzeAndCounts(t *testing.T) {
	apply := strings.Join([]string{
		`CREATE TABLE "clients" (uuid UUID PRIMARY KEY);`,
		`ALTER TABLE "leads" ADD COLUMN "tz" VARCHAR(64);`,
		`ALTER TABLE "orders" DROP COLUMN "legacy_ref";`,
		`DROP TABLE "audit_log_2023";`,
		`DROP INDEX "idx_leads_email";`,
		`ALTER TABLE "leads" ALTER COLUMN "email" SET NOT NULL;`,
	}, "\n")

	p := Analyze(apply)
	if len(p.Statements) != 6 {
		t.Fatalf("got %d statements, want 6", len(p.Statements))
	}
	// Index must be execution order, so a reader can find a called-out statement
	// in the full listing.
	for i, s := range p.Statements {
		if s.Index != i+1 {
			t.Fatalf("statement %d has Index %d", i, s.Index)
		}
	}
	c := p.Counts()
	want := Counts{Total: 6, Additive: 2, DataLoss: 2, ConstraintLoss: 1, Narrowing: 1}
	if c != want {
		t.Fatalf("counts = %+v, want %+v", c, want)
	}
	if !p.HasDestructive() {
		t.Fatal("HasDestructive() = false")
	}
	if got := len(p.Destructive()); got != 2 {
		t.Fatalf("Destructive() returned %d, want 2", got)
	}
	if p.Destructive()[0].Index != 3 || p.Destructive()[1].Index != 4 {
		t.Fatalf("destructive statements are out of execution order: %d, %d",
			p.Destructive()[0].Index, p.Destructive()[1].Index)
	}
}

func TestAnalyzeEmpty(t *testing.T) {
	p := Analyze("")
	if !p.Empty() {
		t.Fatal("Empty() = false for an empty plan")
	}
	if p.HasDestructive() {
		t.Fatal("an empty plan claims to be destructive")
	}
	if got := p.SummaryLine(); !strings.Contains(got, "No changes") {
		t.Fatalf("SummaryLine() = %q", got)
	}
	if p.RenderDestructive() != "" || p.RenderStatements() != "" || p.TransactionalWarning(EngineMySQL) != "" {
		t.Fatal("an empty plan rendered non-empty output")
	}
}

func TestRendering(t *testing.T) {
	p := Analyze(`CREATE TABLE a (id INT);
ALTER TABLE "public"."orders" DROP COLUMN "legacy_ref";
DROP INDEX idx_a;`)

	summary := p.SummaryLine()
	for _, want := range []string{"3 statements", "1 additive", "1 DESTRUCTIVE", "1 index/constraint drop"} {
		if !strings.Contains(summary, want) {
			t.Errorf("SummaryLine() = %q, missing %q", summary, want)
		}
	}

	dest := p.RenderDestructive()
	if !strings.Contains(dest, `DROP COLUMN "legacy_ref"`) {
		t.Errorf("RenderDestructive() lost the SQL: %q", dest)
	}
	// The annotation borrows pg-schema-diff's vocabulary on purpose.
	if !strings.Contains(dest, "-- HAZARD DELETES_DATA:") {
		t.Errorf("RenderDestructive() = %q, want a DELETES_DATA hazard line", dest)
	}
	if !strings.Contains(dest, "   2. ") {
		t.Errorf("RenderDestructive() lost the execution-order index: %q", dest)
	}
	// An index drop must not be dressed up as data loss.
	if strings.Count(dest, "HAZARD") != 1 {
		t.Errorf("RenderDestructive() included a non-destructive statement: %q", dest)
	}

	all := p.RenderStatements()
	if !strings.Contains(all, "HAZARD INDEX_DROPPED") {
		t.Errorf("RenderStatements() = %q, want an INDEX_DROPPED hazard", all)
	}
	if !strings.Contains(all, "   1. CREATE TABLE a (id INT)") {
		t.Errorf("RenderStatements() = %q, missing the additive statement", all)
	}

	if !strings.Contains(p.TransactionalWarning(EngineMySQL), "ONE AT A TIME") {
		t.Errorf("TransactionalWarning() = %q", p.TransactionalWarning(EngineMySQL))
	}
}

// The statement is verbatim from the round-6 v3 plan. It costs three different
// things at once, and the annotation used to name one of them: the reader was told
// they would lose moisture_pct, and not that warehouse_bin — char(24) in the live
// database — was being retyped to int, which coerces every value in it to 0. The
// classification (destructive) was right; the human-readable reason was a third of
// the answer, which is worse than a vague one because it reads as complete.
func TestBundledAlterReportsEveryHazard(t *testing.T) {
	const sql = "ALTER TABLE `lot` DROP KEY `idx_lot_process_organic`, DROP COLUMN `moisture_pct`, MODIFY COLUMN `warehouse_bin` int"

	p := Analyze(sql)
	if len(p.Statements) != 1 {
		t.Fatalf("Analyze() = %d statements, want 1", len(p.Statements))
	}
	s := p.Statements[0]

	// The gate still keys on the worst action: this is one destructive statement.
	if s.Severity != SeverityDataLoss || s.Kind != KindDropColumn {
		t.Errorf("summary = %q/%q, want %q/%q", s.Kind, s.Severity, KindDropColumn, SeverityDataLoss)
	}
	if !p.HasDestructive() || len(p.Destructive()) != 1 {
		t.Errorf("destructive = %d statements, want 1", len(p.Destructive()))
	}

	if len(s.Hazards) != 3 {
		t.Fatalf("hazards = %d, want 3 (index drop, column drop, retype): %+v", len(s.Hazards), s.Hazards)
	}
	// Worst first, whatever order the clauses appear in — the DROP KEY is written
	// before the DROP COLUMN in this statement.
	if s.Hazards[0].Severity != SeverityDataLoss {
		t.Errorf("hazards are not worst-first: %+v", s.Hazards)
	}

	got := map[Severity]Hazard{}
	for _, h := range s.Hazards {
		got[h.Severity] = h
	}
	for sev, want := range map[Severity]string{
		SeverityDataLoss:       "moisture_pct",
		SeverityConstraintLoss: "index",
		SeverityNarrowing:      "warehouse_bin",
	} {
		h, ok := got[sev]
		if !ok {
			t.Errorf("no %q hazard in %+v", sev, s.Hazards)
			continue
		}
		if !strings.Contains(h.Reason, want) {
			t.Errorf("%q hazard reads %q, missing %q", sev, h.Reason, want)
		}
	}
	if obj := got[SeverityNarrowing].Object; obj != "lot.warehouse_bin" {
		t.Errorf("retype hazard object = %q, want lot.warehouse_bin", obj)
	}

	// Every one of them reaches the reader, in the plan and in the called-out
	// destructive block.
	for _, render := range []struct {
		name string
		out  string
	}{
		{"RenderStatements", p.RenderStatements()},
		{"RenderDestructive", p.RenderDestructive()},
	} {
		for _, want := range []string{
			"-- HAZARD DELETES_DATA: drops moisture_pct from lot and every value in it",
			"-- HAZARD INDEX_DROPPED: drops an index on lot",
			"-- HAZARD CORRECTNESS: redefines warehouse_bin on lot; can truncate or fail depending on the values already stored",
		} {
			if !strings.Contains(render.out, want) {
				t.Errorf("%s() is missing:\n  %s\ngot:\n%s", render.name, want, render.out)
			}
		}
	}
}

// sql-push now requests a transaction, so what a partial failure costs depends on the
// engine AND on what the plan contains. One unconditional sentence can no longer be
// honest for every plan, and the reassuring direction is the expensive one to get
// wrong: a reader who believes a failed migration rolled back will not go and check.
func TestTransactionalWarningIsEngineAndContentAware(t *testing.T) {
	plain := Analyze("CREATE TABLE a (id INT);\nALTER TABLE b ADD COLUMN c INT;")

	// Postgres gets real atomicity.
	pg := plain.TransactionalWarning(EnginePostgres)
	if !strings.Contains(pg, "TRANSACTION") || strings.Contains(pg, "ONE AT A TIME") {
		t.Errorf("postgres warning should promise a rollback: %q", pg)
	}
	if !plain.Transactional(EnginePostgres) {
		t.Error("an ordinary postgres plan should report as transactional")
	}

	// MySQL never does: the transaction is opened, but DDL commits implicitly.
	my := plain.TransactionalWarning(EngineMySQL)
	if !strings.Contains(my, "ONE AT A TIME") || !strings.Contains(my, "DDL") {
		t.Errorf("mysql warning should explain the implicit commit: %q", my)
	}
	if plain.Transactional(EngineMySQL) {
		t.Error("a mysql plan must never report as transactional")
	}

	// One statement Postgres cannot run inside a transaction block downgrades the
	// WHOLE batch to statement-at-a-time, so the plan loses its atomicity — not just
	// that statement.
	for _, sql := range []string{
		"CREATE TABLE a (id INT);\nCREATE INDEX CONCURRENTLY idx_a ON a (id)",
		"CREATE TABLE a (id INT);\nVACUUM FULL a",
		"CREATE TABLE a (id INT);\nALTER SYSTEM SET work_mem = '64MB'",
	} {
		p := Analyze(sql)
		got := p.TransactionalWarning(EnginePostgres)
		if !strings.Contains(got, "do NOT run in a transaction") {
			t.Errorf("plan %q should lose atomicity, got %q", sql, got)
		}
		if !strings.Contains(got, "Statement 2") {
			t.Errorf("warning should name the offending statement: %q", got)
		}
		if p.Transactional(EnginePostgres) {
			t.Errorf("plan %q reported as transactional", sql)
		}
	}

	// An engine nobody resolved gets the conservative answer, never the reassuring one.
	unknown := plain.TransactionalWarning(EngineUnknown)
	if strings.Contains(unknown, "rolls back") {
		t.Errorf("an unknown engine must not promise a rollback: %q", unknown)
	}
	if plain.Transactional(EngineUnknown) {
		t.Error("an unknown engine must not report as transactional")
	}
}

func TestChurnNote(t *testing.T) {
	churny := Analyze("ALTER TABLE `a` MODIFY COLUMN `x` VARCHAR(512);\nALTER TABLE `b` MODIFY COLUMN `y` VARCHAR(512);\nCREATE TABLE c (id INT);")
	if got := churny.ChurnNote(); !strings.Contains(got, "2 of 3") {
		t.Errorf("ChurnNote() = %q, want it to count the column redefinitions", got)
	}
	// A destructive plan with none of the churn shapes in it has nothing to explain.
	clean := Analyze("DROP TABLE a;")
	if got := clean.ChurnNote(); got != "" {
		t.Errorf("ChurnNote() = %q, want empty", got)
	}
}

// The note used to count column redefinitions only. A MySQL plan that never converges
// is typically an index whose TYPE the reconstruction lost — the differ proposes to
// drop it and add it back on every single run — and neither half of that pair is a
// column redefinition, so a plan that was pure churn top to bottom got no note at all.
func TestChurnNoteCountsIndexChurn(t *testing.T) {
	p := Analyze("ALTER TABLE `doc` DROP KEY `idx_body`;\n" +
		"ALTER TABLE `doc` ADD FULLTEXT KEY `idx_body` (`body`);\n" +
		"CREATE TABLE c (id INT);")
	got := p.ChurnNote()
	if !strings.Contains(got, "2 of 3") {
		t.Errorf("ChurnNote() = %q, want it to count the index drop/add pair", got)
	}
	if !strings.Contains(got, "index") {
		t.Errorf("ChurnNote() = %q, want it to name index churn", got)
	}

	// Both shapes in one plan are counted together and named separately.
	mixed := Analyze("ALTER TABLE `a` MODIFY COLUMN `x` VARCHAR(512);\n" +
		"DROP INDEX idx_a;\n" +
		"CREATE INDEX idx_a ON a (x);\n" +
		"CREATE TABLE c (id INT);")
	got = mixed.ChurnNote()
	if !strings.Contains(got, "3 of 4") {
		t.Errorf("ChurnNote() = %q, want 3 of 4", got)
	}
	for _, want := range []string{"redefine a column (1)", "drop or add an index (2)"} {
		if !strings.Contains(got, want) {
			t.Errorf("ChurnNote() = %q, missing %q", got, want)
		}
	}
}

// The count belongs either to the lead ("3 of 3 statements") or to the breakdown,
// never to both: printing it in both places produced "3 of 3 statements 3 redefine a
// column" on every MySQL plan. Pinned as whole sentences — this is a defect only a
// reader notices, so only a test that reads the sentence can catch it.
func TestChurnNoteSentenceDoesNotRepeatTheCount(t *testing.T) {
	tail := " — on MySQL those are the two shapes no-op churn takes, and\n" +
		"a statement that reappears on every deploy is almost certainly one of them.\n" +
		"Compare them against the schema you actually have before reading them as changes."

	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			// The reported case: every statement in the plan is a column redefinition.
			name: "all column redefinitions",
			sql: "ALTER TABLE `a` MODIFY COLUMN `x` VARCHAR(512);\n" +
				"ALTER TABLE `b` MODIFY COLUMN `y` VARCHAR(512);\n" +
				"ALTER TABLE `c` MODIFY COLUMN `z` VARCHAR(512);",
			want: "3 of 3 statements redefine a column" + tail,
		},
		{
			name: "index churn only",
			sql: "ALTER TABLE `doc` DROP KEY `idx_body`;\n" +
				"ALTER TABLE `doc` ADD FULLTEXT KEY `idx_body` (`body`);\n" +
				"CREATE TABLE c (id INT);",
			want: "2 of 3 statements drop or add an index" + tail,
		},
		{
			// Both shapes: here the split IS the information, so each count appears
			// exactly once, beside the shape it belongs to.
			name: "both shapes",
			sql: "ALTER TABLE `a` MODIFY COLUMN `x` VARCHAR(512);\n" +
				"DROP INDEX idx_a;\n" +
				"CREATE INDEX idx_a ON a (x);\n" +
				"CREATE TABLE c (id INT);",
			want: "3 of 4 statements redefine a column (1) or drop or add an index (2)" + tail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.sql).ChurnNote()
			if got != tc.want {
				t.Errorf("ChurnNote() =\n%s\nwant\n%s", got, tc.want)
			}
			// No count ever follows the word "statements" — that is the doubling.
			for _, bad := range []string{"statements 1 ", "statements 2 ", "statements 3 "} {
				if strings.Contains(got, bad) {
					t.Errorf("ChurnNote() repeats its count (%q): %q", bad, got)
				}
			}
		})
	}
}

func TestSingleStatementHasNoTransactionWarning(t *testing.T) {
	// With one statement there is no "the ones before it stayed applied" hazard, so
	// the warning would be noise.
	p := Analyze("DROP TABLE a;")
	for _, engine := range []Engine{EngineMySQL, EnginePostgres, EngineUnknown} {
		if got := p.TransactionalWarning(engine); got != "" {
			t.Fatalf("TransactionalWarning(%q) = %q for a single statement", engine, got)
		}
	}
}
