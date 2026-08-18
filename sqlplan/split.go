package sqlplan

import "strings"

// split.go is a port of splitSQLStatements in nuzur-go's
// connection-manager/module/sql-query-manager/split.go, and exists to keep the
// promise Split makes: the preview shows the statements the connection manager
// will actually run. The two must be changed together — a divergence here is a
// plan that lies about what is about to happen to a database.

// Split splits apply SQL into statements exactly the way the connection manager
// will execute it.
//
// A ";" only separates statements when it is not inside a string literal, a
// quoted identifier, a comment or (Postgres) a dollar-quoted body. Splitting on
// every ";" — which this did until a commented migration script was cut
// mid-sentence and the second half of an English sentence was handed to MySQL —
// produces fragments that are not statements, and a preview that classifies
// prose.
//
// Fragments carrying no SQL (a trailing comment, a stray ";;") are dropped, as
// they are by the executor. Statements keep the comments written above them and
// are trimmed for display: leading and trailing whitespace is the one difference
// from what runs, and it changes nothing about what runs.
//
// Anything but EnginePostgres is read with MySQL's rules, which is exactly what
// the executor does with a connection whose type is not Postgres.
func Split(applySQL string, engine Engine) []string {
	s := &scriptSplitter{
		src:       applySQL,
		mysql:     engine != EnginePostgres,
		delimiter: ";",
	}
	return s.split()
}

// scriptSplitter walks a script once, tracking whether the current byte is
// inside something a ";" cannot separate, and whether the statement being
// accumulated has any SQL in it yet.
type scriptSplitter struct {
	src   string
	mysql bool

	// delimiter is ";" until a MySQL DELIMITER directive changes it. Scripts
	// that create triggers or stored procedures rely on it: the routine body
	// is full of semicolons, and without honouring the directive every such
	// script is cut into fragments that cannot parse.
	delimiter string

	out     []string
	start   int  // first byte of the statement being accumulated
	hasCode bool // the statement has something other than whitespace/comments
}

func (s *scriptSplitter) split() []string {
	i := 0
	for i < len(s.src) {
		if !s.hasCode && s.mysql {
			if next, ok := s.readDelimiterDirective(i); ok {
				i = next
				continue
			}
		}

		if s.delimiter != "" && strings.HasPrefix(s.src[i:], s.delimiter) {
			s.flush(i)
			i += len(s.delimiter)
			s.start = i
			continue
		}

		next, code := s.skipAt(i)
		if code {
			s.hasCode = true
		}
		i = next
	}
	s.flush(len(s.src))
	return s.out
}

// skipAt consumes whatever starts at i — a literal, a comment, or a single
// ordinary byte — and reports whether it counted as SQL.
func (s *scriptSplitter) skipAt(i int) (next int, code bool) {
	c := s.src[i]
	switch {
	case c == '\'', c == '"', c == '`' && s.mysql:
		if end, ok := s.scanQuoted(i); ok {
			return end + 1, true
		}
		// Unterminated: the rest of the script belongs to the engine's error,
		// not to a guess about where the literal was meant to end.
		return len(s.src), true

	case c == '-' && s.lineCommentStartsAt(i):
		return s.endOfLine(i), false

	case c == '#' && s.mysql:
		return s.endOfLine(i), false

	case c == '/' && i+1 < len(s.src) && s.src[i+1] == '*':
		end, ok := s.scanBlockComment(i)
		if !ok {
			return len(s.src), false
		}
		// /*! ... */ is MySQL's executable comment: the server runs what is
		// inside it, so a fragment containing nothing else is still a
		// statement.
		executable := s.mysql && i+2 < len(s.src) && s.src[i+2] == '!'
		return end, executable

	case c == '$' && !s.mysql:
		if end, ok := s.scanDollarQuoted(i); ok {
			return end, true
		}
		return i + 1, true

	default:
		return i + 1, !isSQLSpace(c)
	}
}

// flush ends the statement that started at s.start, keeping it only if it has
// SQL in it.
func (s *scriptSplitter) flush(end int) {
	if s.hasCode {
		if statement := strings.TrimSpace(s.src[s.start:end]); statement != "" {
			s.out = append(s.out, statement)
		}
	}
	s.hasCode = false
	s.start = end
}

// lineCommentStartsAt reports whether the "-" at i opens a comment.
//
// Postgres treats "--" as a comment wherever it appears; MySQL requires
// whitespace (or end of input) after it, which is what makes "SELECT 5--2" a
// subtraction there and a comment in Postgres.
func (s *scriptSplitter) lineCommentStartsAt(i int) bool {
	if i+1 >= len(s.src) || s.src[i+1] != '-' {
		return false
	}
	if !s.mysql {
		return true
	}
	return i+2 >= len(s.src) || isSQLSpace(s.src[i+2])
}

func (s *scriptSplitter) endOfLine(i int) int {
	if end := strings.IndexByte(s.src[i:], '\n'); end >= 0 {
		return i + end
	}
	return len(s.src)
}

// scanQuoted returns the index of the quote that closes the one at start.
//
// A doubled quote is an embedded one in both engines. A backslash escapes the
// next byte in MySQL string literals, and in Postgres only inside an E'...' string
// — treating it as an escape in a plain Postgres literal would run the scan
// past the real closing quote of something like 'C:\'.
func (s *scriptSplitter) scanQuoted(start int) (int, bool) {
	quote := s.src[start]
	escapes := s.backslashEscapes(start)
	for i := start + 1; i < len(s.src); i++ {
		switch s.src[i] {
		case '\\':
			if escapes {
				i++ // skip whatever it protects, closing quote included
			}
		case quote:
			if i+1 < len(s.src) && s.src[i+1] == quote {
				i++ // doubled: an embedded quote, not the end
				continue
			}
			return i, true
		}
	}
	return 0, false
}

func (s *scriptSplitter) backslashEscapes(start int) bool {
	switch s.src[start] {
	case '`':
		return false // backquoted identifiers double, they do not escape
	case '"':
		// A MySQL string literal escapes; a Postgres quoted identifier does not.
		return s.mysql
	}
	if s.mysql {
		return true
	}
	// Postgres: only E'...' honours backslash escapes.
	if start == 0 {
		return false
	}
	if e := s.src[start-1]; e != 'E' && e != 'e' {
		return false
	}
	return start-1 == 0 || !isIdentByte(s.src[start-2])
}

// scanBlockComment returns the index just past the comment that opens at start.
// Postgres nests block comments; MySQL does not.
func (s *scriptSplitter) scanBlockComment(start int) (int, bool) {
	depth := 1
	for i := start + 2; i+1 < len(s.src); i++ {
		switch {
		case s.src[i] == '*' && s.src[i+1] == '/':
			depth--
			if depth == 0 {
				return i + 2, true
			}
			i++
		case !s.mysql && s.src[i] == '/' && s.src[i+1] == '*':
			depth++
			i++
		}
	}
	return 0, false
}

// scanDollarQuoted returns the index just past a Postgres dollar-quoted string
// ($$body$$ or $tag$body$tag$), the usual spelling of a function body — which
// is where the semicolons that most need protecting live.
//
// Reports false for anything that is not one, including the "$1" of a
// placeholder and a tag that never closes.
func (s *scriptSplitter) scanDollarQuoted(start int) (int, bool) {
	tagEnd := start + 1
	for tagEnd < len(s.src) && s.src[tagEnd] != '$' {
		if !isIdentByte(s.src[tagEnd]) || (isDigit(s.src[tagEnd]) && tagEnd == start+1) {
			return 0, false
		}
		tagEnd++
	}
	if tagEnd >= len(s.src) {
		return 0, false
	}

	tag := s.src[start : tagEnd+1] // includes both "$"
	body := s.src[tagEnd+1:]
	end := strings.Index(body, tag)
	if end < 0 {
		return 0, false
	}
	return tagEnd + 1 + end + len(tag), true
}

// readDelimiterDirective consumes a MySQL "DELIMITER x" line and returns the
// index to continue from. The directive is a client instruction, not SQL: it
// changes what separates statements from here on and is never sent to the
// server.
func (s *scriptSplitter) readDelimiterDirective(i int) (int, bool) {
	if !atLineStart(s.src, i) {
		return 0, false
	}
	const word = "delimiter"
	if len(s.src)-i <= len(word) || !strings.EqualFold(s.src[i:i+len(word)], word) {
		return 0, false
	}
	rest := s.src[i+len(word):]
	if !isSQLSpace(rest[0]) || rest[0] == '\n' {
		return 0, false
	}

	end := s.endOfLine(i)
	delimiter := strings.TrimSpace(s.src[i+len(word) : end])
	if delimiter == "" {
		return 0, false
	}
	s.delimiter = delimiter
	s.start = end
	return end, true
}

// atLineStart reports whether only whitespace separates i from the start of its
// line, which is the only place the DELIMITER directive is recognised.
func atLineStart(src string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch src[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
		default:
			return false
		}
	}
	return true
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isIdentByte(c byte) bool {
	return c == '_' || isDigit(c) ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
