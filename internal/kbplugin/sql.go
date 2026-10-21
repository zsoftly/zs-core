package kbplugin

import (
	"fmt"
	"regexp"
	"strings"
)

// Statement kinds of a plugin DDL. Every statement of a plugin's DDL must be
// one of these. Core refuses a DDL with any other statement, so it never
// runs a statement it cannot attribute to a table or a domain.
const (
	KindCreateTable = "create_table"
	KindDropTable   = "drop_table"
	KindCreateIndex = "create_index"
	KindAlterTable  = "alter_table"
	KindCreateType  = "create_domain"
)

// Statement is one SQL statement of a plugin DDL. Object is the table or
// domain it acts on, in lower case and without a schema.
type Statement struct {
	SQL    string
	Kind   string
	Object string
}

var (
	reCreateTable = regexp.MustCompile(`^create table (?:if not exists )?(` + identPattern + `)`)
	reDropTable   = regexp.MustCompile(`^drop table (?:if exists )?(` + identPattern + `)(?: cascade| restrict)?$`)
	reCreateIndex = regexp.MustCompile(`^create (?:unique )?index (?:concurrently )?(?:if not exists )?(?:` + identPattern + ` )?on (?:only )?(` + identPattern + `)`)
	reAlterTable  = regexp.MustCompile(`^alter table (?:if exists )?(?:only )?(` + identPattern + `)`)
	reCreateType  = regexp.MustCompile(`^create domain (` + identPattern + `)`)
)

// identPattern matches a table, index or domain name, with an optional
// public schema, quoted or not.
const identPattern = `(?:"?public"?\.)?"?[a-z_][a-z0-9_]*"?`

// ParseDDL splits a DDL text into statements and classifies each one.
// Comments, including MySQL's /*! ... */ comments, are dropped from the
// classification, and a statement that holds only comments is skipped.
func ParseDDL(text string) ([]Statement, error) {
	raw, err := splitSQL(text)
	if err != nil {
		return nil, err
	}
	var out []Statement
	for _, sql := range raw {
		norm := normalize(sql)
		if norm == "" {
			continue
		}
		st := Statement{SQL: strings.TrimSpace(sql)}
		switch {
		case reCreateTable.MatchString(norm):
			st.Kind, st.Object = KindCreateTable, objectName(reCreateTable.FindStringSubmatch(norm)[1])
		case reDropTable.MatchString(norm):
			st.Kind, st.Object = KindDropTable, objectName(reDropTable.FindStringSubmatch(norm)[1])
		case reCreateIndex.MatchString(norm):
			st.Kind, st.Object = KindCreateIndex, objectName(reCreateIndex.FindStringSubmatch(norm)[1])
		case reAlterTable.MatchString(norm):
			st.Kind, st.Object = KindAlterTable, objectName(reAlterTable.FindStringSubmatch(norm)[1])
		case reCreateType.MatchString(norm):
			st.Kind, st.Object = KindCreateType, objectName(reCreateType.FindStringSubmatch(norm)[1])
		default:
			return nil, fmt.Errorf("the DDL holds a statement Core does not apply: %q", shorten(norm, 80))
		}
		out = append(out, st)
	}
	return out, nil
}

// Tables returns the tables a DDL creates, in the order it creates them.
func Tables(stmts []Statement) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range stmts {
		if s.Kind == KindCreateTable && !seen[s.Object] {
			seen[s.Object] = true
			out = append(out, s.Object)
		}
	}
	return out
}

// Domains returns the domains a DDL creates.
func Domains(stmts []Statement) []string {
	var out []string
	for _, s := range stmts {
		if s.Kind == KindCreateType {
			out = append(out, s.Object)
		}
	}
	return out
}

func objectName(s string) string {
	s = strings.ReplaceAll(s, `"`, "")
	s = strings.TrimPrefix(s, "public.")
	return s
}

func shorten(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// normalize removes the comments of a statement, lowers its case and
// collapses its blanks, for classification only.
func normalize(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 3
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(b.String()), " "))
}

// splitSQL splits a script on the semicolons that end statements. It skips
// semicolons inside comments, quoted strings, quoted identifiers and dollar
// quoted bodies.
func splitSQL(text string) ([]string, error) {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '-' && i+1 < len(text) && text[i+1] == '-':
			for i < len(text) && text[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("the DDL holds an unterminated comment")
			}
			i += end + 3
		case c == '\'' || c == '"':
			j := i + 1
			for ; j < len(text); j++ {
				if text[j] == c {
					if j+1 < len(text) && text[j+1] == c {
						j++
						continue
					}
					break
				}
			}
			if j >= len(text) {
				return nil, fmt.Errorf("the DDL holds an unterminated quote")
			}
			i = j
		case c == '$':
			tag := dollarTag(text[i:])
			if tag == "" {
				continue
			}
			end := strings.Index(text[i+len(tag):], tag)
			if end < 0 {
				return nil, fmt.Errorf("the DDL holds an unterminated dollar quote")
			}
			i += len(tag) + end + len(tag) - 1
		case c == ';':
			out = append(out, text[start:i])
			start = i + 1
		}
	}
	if strings.TrimSpace(text[start:]) != "" {
		out = append(out, text[start:])
	}
	return out, nil
}

var reDollarTag = regexp.MustCompile(`^\$[A-Za-z_]*\$`)

func dollarTag(s string) string { return reDollarTag.FindString(s) }
