package sqlplan

import (
	"strings"
	"testing"
)

// The cases are the port's contract: sqlplan.Split has to answer exactly what
// splitSQLStatements answers in nuzur-go's sql-query-manager, or the preview and
// the execution disagree. Keep this table in step with the one there.
func TestSplitDialects(t *testing.T) {
	cases := []struct {
		name   string
		engine Engine
		script string
		want   []string
	}{
		{
			name:   "plain statements",
			engine: EngineMySQL,
			script: "SELECT 1; SELECT 2;",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			// The reported bug: a semicolon in a sentence inside a line comment
			// cut the comment in half and sent the rest to MySQL as SQL.
			name:   "semicolon inside a line comment",
			engine: EngineMySQL,
			script: "-- the biggest tables take the longest; run inside the window.\nALTER TABLE t DROP FOREIGN KEY fk;\nALTER TABLE t ADD CONSTRAINT fk2 FOREIGN KEY (a) REFERENCES u(id);",
			want: []string{
				"-- the biggest tables take the longest; run inside the window.\nALTER TABLE t DROP FOREIGN KEY fk",
				"ALTER TABLE t ADD CONSTRAINT fk2 FOREIGN KEY (a) REFERENCES u(id)",
			},
		},
		{
			name:   "semicolon inside a block comment",
			engine: EngineMySQL,
			script: "/* one; two */ SELECT 1;",
			want:   []string{"/* one; two */ SELECT 1"},
		},
		{
			name:   "nested block comment on postgres",
			engine: EnginePostgres,
			script: "/* outer /* inner; */ still; comment */ SELECT 1;",
			want:   []string{"/* outer /* inner; */ still; comment */ SELECT 1"},
		},
		{
			name:   "hash comment is mysql only",
			engine: EngineMySQL,
			script: "# drop it; really\nSELECT 1;",
			want:   []string{"# drop it; really\nSELECT 1"},
		},
		{
			name:   "semicolon inside a string literal",
			engine: EngineMySQL,
			script: "INSERT INTO t (a) VALUES ('one;two'); SELECT 1;",
			want:   []string{"INSERT INTO t (a) VALUES ('one;two')", "SELECT 1"},
		},
		{
			name:   "doubled quote inside a string literal",
			engine: EnginePostgres,
			script: "INSERT INTO t (a) VALUES ('it''s; fine'); SELECT 1;",
			want:   []string{"INSERT INTO t (a) VALUES ('it''s; fine')", "SELECT 1"},
		},
		{
			name:   "backslash escape in a mysql literal",
			engine: EngineMySQL,
			script: `INSERT INTO t (a) VALUES ('a\'; b'); SELECT 1;`,
			want:   []string{`INSERT INTO t (a) VALUES ('a\'; b')`, "SELECT 1"},
		},
		{
			// In Postgres the backslash is an ordinary character, so the
			// literal ends at the second quote — reading it as an escape would
			// swallow the separator.
			name:   "backslash is literal in a postgres string",
			engine: EnginePostgres,
			script: `INSERT INTO t (a) VALUES ('C:\'); SELECT 1;`,
			want:   []string{`INSERT INTO t (a) VALUES ('C:\')`, "SELECT 1"},
		},
		{
			name:   "postgres E string honours the escape",
			engine: EnginePostgres,
			script: `SELECT E'a\'; b'; SELECT 1;`,
			want:   []string{`SELECT E'a\'; b'`, "SELECT 1"},
		},
		{
			name:   "semicolon inside quoted identifiers",
			engine: EngineMySQL,
			script: "SELECT `we;ird` FROM t; SELECT 1;",
			want:   []string{"SELECT `we;ird` FROM t", "SELECT 1"},
		},
		{
			name:   "postgres dollar quoted function body",
			engine: EnginePostgres,
			script: "CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql; SELECT 1;",
			want: []string{
				"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql",
				"SELECT 1",
			},
		},
		{
			name:   "postgres tagged dollar quote",
			engine: EnginePostgres,
			script: "CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql; SELECT 2;",
			want: []string{
				"CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql",
				"SELECT 2",
			},
		},
		{
			name:   "dollar placeholders are not dollar quotes",
			engine: EnginePostgres,
			script: "UPDATE t SET a = $1 WHERE id = $2; SELECT 1;",
			want:   []string{"UPDATE t SET a = $1 WHERE id = $2", "SELECT 1"},
		},
		{
			name:   "mysql delimiter directive keeps a routine whole",
			engine: EngineMySQL,
			script: "DELIMITER //\nCREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN SET NEW.x = 1; SET NEW.y = 2; END//\nDELIMITER ;\nSELECT 1;",
			want: []string{
				"CREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN SET NEW.x = 1; SET NEW.y = 2; END",
				"SELECT 1",
			},
		},
		{
			name:   "trailing comment is not a statement",
			engine: EngineMySQL,
			script: "SELECT 1;\n-- done for today\n",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "empty fragments are dropped",
			engine: EnginePostgres,
			script: ";;SELECT 1;;\n\n;",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "comments only",
			engine: EnginePostgres,
			script: "-- nothing here\n/* nor here */\n",
			want:   nil,
		},
		{
			name:   "mysql executable comment is a statement",
			engine: EngineMySQL,
			script: "/*!40101 SET NAMES utf8 */;\nSELECT 1;",
			want:   []string{"/*!40101 SET NAMES utf8 */", "SELECT 1"},
		},
		{
			// MySQL needs whitespace after "--" for it to be a comment, so this
			// is a subtraction and the ";" that follows is the separator.
			name:   "mysql double minus without whitespace",
			engine: EngineMySQL,
			script: "SELECT 5--2; SELECT 1;",
			want:   []string{"SELECT 5--2", "SELECT 1"},
		},
		{
			// Postgres reads the same text as a comment, so everything to the
			// end of the line — the ";" included — is prose.
			name:   "postgres double minus without whitespace",
			engine: EnginePostgres,
			script: "SELECT 5--2; SELECT 1;",
			want:   []string{"SELECT 5--2; SELECT 1;"},
		},
		{
			name:   "no trailing delimiter",
			engine: EngineMySQL,
			script: "SELECT 1;\nSELECT 2",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			// An unterminated literal is the engine's to reject: splitting
			// inside it would be a guess, so the rest of the script stays put.
			name:   "unterminated string literal",
			engine: EngineMySQL,
			script: "SELECT 'oops; SELECT 1;",
			want:   []string{"SELECT 'oops; SELECT 1;"},
		},
		{
			name:   "unterminated block comment",
			engine: EngineMySQL,
			script: "SELECT 1; /* oops; SELECT 2;",
			want:   []string{"SELECT 1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Split(tc.script, tc.engine)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("statement %d:\n got %q\nwant %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A migration script written the way a person writes one — comments, prose,
// semicolons in the prose — previews as the statements it actually contains.
func TestSplitACommentedMigration(t *testing.T) {
	script := `-- v3.0 English schema standardization — out-of-band rename script.
--
-- RUN ORDER (maintenance window):
--   1. Scale the API to 0; the old binary queries the old names.
RENAME TABLE
  ` + "`luchador`" + ` TO ` + "`wrestler`" + `,
  ` + "`lucha`" + `    TO ` + "`match`" + `;

-- FK names are the old relationship identifiers. MySQL cannot rename an FK;
-- DROP + ADD. The ADD re-validates (a scan, not metadata-only) — the biggest
-- tables take the longest; run inside the window. Referential actions copied
-- from the deployed DDL.
ALTER TABLE ` + "`promotion`" + ` DROP FOREIGN KEY ` + "`promocion_succeeds_promocion`" + `;
UPDATE ` + "`wrestler`" + ` SET ` + "`links`" + ` = REPLACE(` + "`links`" + `, '"tipo":', '"type":') WHERE ` + "`links`" + ` LIKE '%"tipo":%';
-- stipulation_tag.category held one Spanish value.
UPDATE ` + "`stipulation_tag`" + ` SET ` + "`category`" + ` = 'wager' WHERE ` + "`category`" + ` = 'apuesta';
`

	got := Split(script, EngineMySQL)
	if len(got) != 4 {
		t.Fatalf("got %d statements, want 4:\n%q", len(got), got)
	}
	for i, want := range []string{"RENAME TABLE", "ALTER TABLE", "UPDATE", "UPDATE"} {
		code := strings.TrimSpace(stripLeadingLineComments(got[i]))
		if !strings.HasPrefix(code, want) {
			t.Errorf("statement %d should start with %q, got %q", i, want, code)
		}
	}
}

// stripLeadingLineComments drops the comment lines a statement carries with it,
// so a test can assert on the SQL underneath.
func stripLeadingLineComments(statement string) string {
	for {
		trimmed := strings.TrimLeft(statement, " \t\r\n")
		if !strings.HasPrefix(trimmed, "--") {
			return trimmed
		}
		nl := strings.IndexByte(trimmed, '\n')
		if nl < 0 {
			return ""
		}
		statement = trimmed[nl+1:]
	}
}

// The preview classifies statements, so a bad split does not just look wrong —
// it labels prose. A default containing a semicolon used to become two
// fragments, the second of which ("b') ...") was classified as KindOther and
// counted as a statement nobody was about to run.
func TestAnalyzeDoesNotClassifyHalvesOfAStatement(t *testing.T) {
	p := Analyze(`ALTER TABLE "notes" ADD COLUMN sep VARCHAR(8) DEFAULT 'a;b';
DROP TABLE "audit_log";`, EnginePostgres)

	if len(p.Statements) != 2 {
		t.Fatalf("got %d statements, want 2: %#v", len(p.Statements), p.Statements)
	}
	if p.Statements[0].Kind != KindAddColumn {
		t.Errorf("first statement = %q, want an add-column", p.Statements[0].Kind)
	}
	if !strings.Contains(p.Statements[0].SQL, "'a;b'") {
		t.Errorf("the default should have survived intact, got %q", p.Statements[0].SQL)
	}
	if !p.HasDestructive() {
		t.Error("the DROP TABLE should still be flagged destructive")
	}
}
