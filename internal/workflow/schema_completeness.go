package workflow

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var createTableHeaderRe = regexp.MustCompile(`(?i)^CREATE TABLE IF NOT EXISTS (\w+)\s*\(`)

// verifySchemaCompleteness checks that every column named in a CREATE TABLE
// block actually exists on the real table, via PRAGMA table_info, after all
// of init's CREATE TABLE and ALTER TABLE statements have run.
//
// This exists because of a real incident: a column added directly to a
// CREATE TABLE string (instead of also getting an ALTER TABLE ... ADD COLUMN
// migration line) works fine for a brand-new database, since the column is
// already in the freshly created table — but CREATE TABLE IF NOT EXISTS is a
// silent no-op against any database where that table already existed under
// the OLD shape, so the column never actually gets added there. The gap only
// surfaced as a "no such column" error from whatever unrelated query
// happened to reference it first, days later, against a real user's
// pre-existing database. This check catches that class of mistake
// immediately and clearly at Store.Open time instead.
func verifySchemaCompleteness(db *sql.DB, ddls ...string) error {
	var missing []string
	for _, ddl := range ddls {
		for table, cols := range parseCreateTableColumns(ddl) {
			actual, err := actualColumns(db, table)
			if err != nil {
				return fmt.Errorf("schema completeness check: %w", err)
			}
			if len(actual) == 0 {
				// Table doesn't exist at all — not this check's job to say why.
				continue
			}
			for _, col := range cols {
				if !actual[col] {
					missing = append(missing, table+"."+col)
				}
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("schema is out of date, missing column(s) %s — add an ALTER TABLE ... ADD COLUMN migration for each in Store.init", strings.Join(missing, ", "))
}

func actualColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// parseCreateTableColumns extracts, for every "CREATE TABLE IF NOT EXISTS
// <name> (...)" block in ddl, the list of column names declared inside it.
// It tracks paren depth line by line rather than matching the whole block
// with one greedy/non-greedy regex, since column and table constraints
// (UNIQUE(a, b), FOREIGN KEY(...), etc.) can contain their own parens.
func parseCreateTableColumns(ddl string) map[string][]string {
	tables := map[string][]string{}
	var currentTable string
	depth := 0
	for _, raw := range strings.Split(ddl, "\n") {
		line := strings.TrimSpace(raw)
		if currentTable == "" {
			if m := createTableHeaderRe.FindStringSubmatch(line); m != nil {
				currentTable = m[1]
				depth = strings.Count(line, "(") - strings.Count(line, ")")
			}
			continue
		}
		depth += strings.Count(line, "(") - strings.Count(line, ")")
		if depth <= 0 {
			currentTable = ""
			continue
		}
		clean := strings.TrimSuffix(line, ",")
		if clean == "" {
			continue
		}
		upper := strings.ToUpper(clean)
		switch {
		case strings.HasPrefix(upper, "PRIMARY KEY"),
			strings.HasPrefix(upper, "FOREIGN KEY"),
			strings.HasPrefix(upper, "UNIQUE"),
			strings.HasPrefix(upper, "CHECK"),
			strings.HasPrefix(upper, "CONSTRAINT"):
			continue
		}
		if fields := strings.Fields(clean); len(fields) > 0 {
			tables[currentTable] = append(tables[currentTable], fields[0])
		}
	}
	return tables
}
