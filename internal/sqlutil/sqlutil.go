// Package sqlutil has dialect-light SQL helpers: fingerprinting, statement
// kind, table extraction, parameter inlining and schema rewriting.
package sqlutil

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

var (
	reComment  = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)
	reString   = regexp.MustCompile(`'(?:[^']|'')*'`)
	reNumber   = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	reParam    = regexp.MustCompile(`\$\d+|\?`)
	reInList   = regexp.MustCompile(`(?i)\(\s*\?(?:\s*,\s*\?)+\s*\)`)
	reValues   = regexp.MustCompile(`(?i)values\s*\(\s*[?,\s]+\)(?:\s*,\s*\(\s*[?,\s]+\))+`)
	reSpace    = regexp.MustCompile(`\s+`)
	reTables   = regexp.MustCompile("(?i)\\b(?:from|join|update|into)\\s+(?:only\\s+)?[`\"]?(\\w+)[`\"]?(?:\\.[`\"]?(\\w+)[`\"]?)?")
	reDeleteT  = regexp.MustCompile("(?i)^\\s*delete\\s+from\\s+[`\"]?(\\w+)[`\"]?(?:\\.[`\"]?(\\w+)[`\"]?)?")
	rePgParam  = regexp.MustCompile(`\$(\d+)`)
	reLimitNum = regexp.MustCompile(`(?i)\blimit\s+(\d+)`)
)

// Normalize strips comments/literals so equivalent statements compare equal.
func Normalize(sql string) string {
	s := reComment.ReplaceAllString(sql, " ")
	s = reString.ReplaceAllString(s, "?")
	s = reParam.ReplaceAllString(s, "?") // before numbers, or $1 becomes $?
	s = reNumber.ReplaceAllString(s, "?")
	s = reInList.ReplaceAllString(s, "(?)")
	s = reValues.ReplaceAllString(s, "values (?)")
	s = reSpace.ReplaceAllString(strings.TrimSpace(s), " ")
	return strings.ToLower(s)
}

func Fingerprint(sql string) string {
	h := sha1.Sum([]byte(Normalize(sql)))
	return hex.EncodeToString(h[:4])
}

func Kind(sql string) string {
	s := strings.ToLower(strings.TrimSpace(reComment.ReplaceAllString(sql, " ")))
	switch {
	case strings.HasPrefix(s, "select"), strings.HasPrefix(s, "with"), strings.HasPrefix(s, "table"):
		if strings.HasPrefix(s, "with") {
			for _, k := range []string{"insert", "update", "delete"} {
				if strings.Contains(s, ") "+k+" ") {
					return k
				}
			}
		}
		if s == "select 1" || strings.HasPrefix(s, "select version()") || strings.HasPrefix(s, "select @@") || strings.HasPrefix(s, "select current_") || strings.HasPrefix(s, "select pg_") {
			return "other"
		}
		return "select"
	case strings.HasPrefix(s, "insert"), strings.HasPrefix(s, "replace"):
		return "insert"
	case strings.HasPrefix(s, "update"):
		return "update"
	case strings.HasPrefix(s, "delete"):
		return "delete"
	}
	return "other"
}

// Tables returns unqualified table names referenced by FROM/JOIN/UPDATE/INTO.
func Tables(sql string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m []string) {
		t := m[1]
		if m[2] != "" {
			t = m[2]
		}
		t = strings.ToLower(t)
		if t == "select" || t == "lateral" || t == "unnest" || t == "generate_series" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, m := range reTables.FindAllStringSubmatch(sql, -1) {
		add(m)
	}
	if m := reDeleteT.FindStringSubmatch(sql); m != nil {
		add(m)
	}
	return out
}

// TargetTable returns the table modified by INSERT/UPDATE/DELETE.
func TargetTable(sql string) string {
	s := strings.TrimSpace(sql)
	re := regexp.MustCompile("(?i)^\\s*(?:insert\\s+into|replace\\s+into|update|delete\\s+from)\\s+(?:only\\s+)?[`\"]?(\\w+)[`\"]?(?:\\.[`\"]?(\\w+)[`\"]?)?")
	if m := re.FindStringSubmatch(s); m != nil {
		if m[2] != "" {
			return strings.ToLower(m[2])
		}
		return strings.ToLower(m[1])
	}
	return ""
}

// InlinePG replaces $n with quoted literals (untyped, so the server infers
// types as it did for the original Parse). "NULL" stays NULL.
func InlinePG(sql string, params []string, nulls map[int]bool) string {
	return rePgParam.ReplaceAllStringFunc(sql, func(m string) string {
		i, _ := strconv.Atoi(m[1:])
		if i < 1 || i > len(params) {
			return m
		}
		if nulls[i-1] {
			return "NULL"
		}
		return "'" + strings.ReplaceAll(params[i-1], "'", "''") + "'"
	})
}

// RewriteSchema replaces qualified references schema.table → target.table
// for the given tables (handles quoting styles of PG and MySQL).
func RewriteSchema(sql, schema, target string, tables []string) string {
	for _, t := range tables {
		re := regexp.MustCompile("(?i)[`\"]?\\b" + regexp.QuoteMeta(schema) + "\\b[`\"]?\\s*\\.\\s*[`\"]?\\b" + regexp.QuoteMeta(t) + "\\b[`\"]?")
		sql = re.ReplaceAllString(sql, target+"."+t)
	}
	return sql
}

func LimitValue(sql string) int {
	if m := reLimitNum.FindStringSubmatch(sql); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v
	}
	return 0
}

// WhereProbe converts UPDATE/DELETE into an equivalent SELECT of the target
// rows (for MySQL, where EXPLAIN ANALYZE doesn't support single-table DML).
func WhereProbe(sql string) string {
	s := strings.TrimSpace(sql)
	up := regexp.MustCompile(`(?is)^update\s+(\S+)\s+set\s+.*?\s+where\s+(.*)$`)
	del := regexp.MustCompile(`(?is)^delete\s+from\s+(\S+)\s+where\s+(.*)$`)
	if m := up.FindStringSubmatch(s); m != nil {
		return "SELECT 1 FROM " + m[1] + " WHERE " + m[2]
	}
	if m := del.FindStringSubmatch(s); m != nil {
		return "SELECT 1 FROM " + m[1] + " WHERE " + m[2]
	}
	return ""
}
