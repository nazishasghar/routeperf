package sqlutil

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"

	pgq "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/reflect/protoreflect"
	"vitess.io/vitess/go/vt/sqlparser"
)

// Ref is one table reference in a statement (CTE names excluded).
type Ref struct {
	Schema string `json:"schema,omitempty"` // as written; "" when unqualified
	Name   string `json:"name"`
	Alias  string `json:"alias,omitempty"`
	start  int    // byte span of the qualified name in the source (Postgres)
	end    int
}

// Parsed is the result of parsing one statement with the dialect's real
// grammar (libpg_query for Postgres, Vitess for MySQL).
type Parsed struct {
	OK          bool
	Kind        string // select | insert | update | delete | other
	Fingerprint string
	Refs        []Ref
	Target      Ref // INSERT/UPDATE/DELETE target; Name "" if none
}

// Names returns the lowercase table names referenced, deduplicated.
func (p Parsed) Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range p.Refs {
		n := strings.ToLower(r.Name)
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

var (
	cacheMu sync.Mutex
	cache   = map[string]Parsed{}
)

// Parse parses sql for dialect ("postgres" or "mysql"), falling back to the
// regex helpers when the grammar rejects the statement. Results are cached.
func Parse(dialect, sql string) Parsed {
	key := dialect + "\x00" + sql
	cacheMu.Lock()
	if p, ok := cache[key]; ok {
		cacheMu.Unlock()
		return p
	}
	cacheMu.Unlock()
	var p Parsed
	var err error
	switch dialect {
	case "postgres":
		p, err = parsePG(sql)
	case "mysql":
		p, err = parseMy(sql)
	default:
		err = errors.New("unknown dialect")
	}
	if err != nil {
		p = Parsed{Kind: Kind(sql), Fingerprint: Fingerprint(sql)}
		for _, t := range Tables(sql) {
			p.Refs = append(p.Refs, Ref{Name: t})
		}
		if t := TargetTable(sql); t != "" {
			p.Target = Ref{Name: t}
		}
	}
	cacheMu.Lock()
	if len(cache) > 100000 {
		cache = map[string]Parsed{}
	}
	cache[key] = p
	cacheMu.Unlock()
	return p
}

// Rewrite replaces table references: fn receives each reference and returns
// the new schema and name (ok=false keeps it). Errors when the statement
// can't be parsed, so callers can fall back.
func Rewrite(dialect, sql string, fn func(Ref) (schema, name string, ok bool)) (string, error) {
	switch dialect {
	case "postgres":
		return rewritePG(sql, fn)
	case "mysql":
		return rewriteMy(sql, fn)
	}
	return "", errors.New("unknown dialect")
}

// ---------------------------------------------------------------- Postgres

func parsePG(sql string) (Parsed, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return Parsed{}, err
	}
	if len(tree.Stmts) == 0 || tree.Stmts[0].Stmt == nil {
		return Parsed{}, errors.New("empty statement")
	}
	p := Parsed{OK: true}
	if fp, err := pgquery.Fingerprint(sql); err == nil {
		p.Fingerprint = fp
	} else {
		p.Fingerprint = Fingerprint(sql)
	}
	refs, dml := pgRefs(tree.Stmts[0].Stmt, sql)
	p.Refs = refs
	switch n := tree.Stmts[0].Stmt.Node.(type) {
	case *pgq.Node_SelectStmt:
		p.Kind = "select"
		if dml != "" {
			p.Kind = dml
		} else if len(refs) == 0 {
			p.Kind = "other"
		}
	case *pgq.Node_InsertStmt:
		p.Kind, p.Target = "insert", pgRef(n.InsertStmt.Relation, sql)
	case *pgq.Node_UpdateStmt:
		p.Kind, p.Target = "update", pgRef(n.UpdateStmt.Relation, sql)
	case *pgq.Node_DeleteStmt:
		p.Kind, p.Target = "delete", pgRef(n.DeleteStmt.Relation, sql)
	case *pgq.Node_MergeStmt:
		p.Kind, p.Target = "update", pgRef(n.MergeStmt.Relation, sql)
	default:
		p.Kind = "other"
	}
	return p, nil
}

func pgRef(rv *pgq.RangeVar, sql string) Ref {
	if rv == nil {
		return Ref{}
	}
	r := Ref{Schema: rv.Schemaname, Name: rv.Relname}
	if rv.Alias != nil {
		r.Alias = rv.Alias.Aliasname
	}
	r.start, r.end = identSpan(sql, int(rv.Location))
	return r
}

// pgRefs walks the parse tree for RangeVars, skipping unqualified references
// to CTE names, and reports the kind of a data-modifying CTE if any.
func pgRefs(root *pgq.Node, sql string) ([]Ref, string) {
	ctes := map[string]bool{}
	var vars []*pgq.RangeVar
	dml := ""
	var walk func(m protoreflect.Message)
	walk = func(m protoreflect.Message) {
		switch x := m.Interface().(type) {
		case *pgq.RangeVar:
			vars = append(vars, x)
		case *pgq.CommonTableExpr:
			ctes[x.Ctename] = true
			if x.Ctequery != nil {
				switch x.Ctequery.Node.(type) {
				case *pgq.Node_InsertStmt:
					dml = "insert"
				case *pgq.Node_UpdateStmt:
					dml = "update"
				case *pgq.Node_DeleteStmt:
					dml = "delete"
				}
			}
		}
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					walk(l.Get(i).Message())
				}
			case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
				walk(v.Message())
			}
			return true
		})
	}
	walk(root.ProtoReflect())
	var out []Ref
	for _, rv := range vars {
		if rv.Schemaname == "" && ctes[rv.Relname] {
			continue
		}
		out = append(out, pgRef(rv, sql))
	}
	return out, dml
}

// identSpan returns the byte span of a (possibly qualified, possibly quoted)
// name starting at loc, e.g. `public."Orders"`, skipping a leading ONLY.
func identSpan(s string, loc int) (int, int) {
	if loc < 0 || loc >= len(s) {
		return -1, -1
	}
	i := loc
	if len(s) >= i+5 && strings.EqualFold(s[i:i+4], "only") && (s[i+4] == ' ' || s[i+4] == '\t' || s[i+4] == '\n') {
		i += 4
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
			i++
		}
	}
	start := i
	for part := 0; part < 3; part++ {
		j := scanIdent(s, i)
		if j == i {
			break
		}
		i = j
		k := i
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
			k++
		}
		if k < len(s) && s[k] == '.' {
			k++
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
				k++
			}
			i = k
			continue
		}
		break
	}
	if i == start {
		return -1, -1
	}
	return start, i
}

func scanIdent(s string, i int) int {
	if i >= len(s) {
		return i
	}
	if s[i] == '"' {
		for j := i + 1; j < len(s); j++ {
			if s[j] == '"' {
				if j+1 < len(s) && s[j+1] == '"' {
					j++
					continue
				}
				return j + 1
			}
		}
		return i
	}
	j := i
	for j < len(s) && (s[j] == '_' || s[j] == '$' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9' || s[j] >= 0x80) {
		j++
	}
	return j
}

// QuotePG quotes an identifier for Postgres.
func QuotePG(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func rewritePG(sql string, fn func(Ref) (string, string, bool)) (string, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return "", err
	}
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	moved := map[[2]string]string{} // (schema, name) written → new schema
	for _, st := range tree.Stmts {
		refs, _ := pgRefs(st.Stmt, sql)
		for _, r := range refs {
			s, n, ok := fn(r)
			if !ok {
				continue
			}
			if r.start < 0 {
				return "", errors.New("cannot locate table reference " + r.Name)
			}
			txt := QuotePG(n)
			if s != "" {
				txt = QuotePG(s) + "." + txt
			}
			edits = append(edits, edit{r.start, r.end, txt})
			if r.Schema != "" {
				moved[[2]string{strings.ToLower(r.Schema), strings.ToLower(r.Name)}] = s
			}
		}
	}
	if len(moved) > 0 { // schema-qualified column refs: schema.table.col
		var cols []*pgq.ColumnRef
		var walk func(m protoreflect.Message)
		walk = func(m protoreflect.Message) {
			if c, ok := m.Interface().(*pgq.ColumnRef); ok && len(c.Fields) == 3 {
				cols = append(cols, c)
			}
			m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
				if fd.IsList() && fd.Kind() == protoreflect.MessageKind {
					for i := 0; i < v.List().Len(); i++ {
						walk(v.List().Get(i).Message())
					}
				} else if fd.Kind() == protoreflect.MessageKind && !fd.IsMap() {
					walk(v.Message())
				}
				return true
			})
		}
		for _, st := range tree.Stmts {
			walk(st.Stmt.ProtoReflect())
		}
		for _, c := range cols {
			s0, s1 := c.Fields[0].GetString_(), c.Fields[1].GetString_()
			if s0 == nil || s1 == nil {
				continue
			}
			ns, ok := moved[[2]string{strings.ToLower(s0.Sval), strings.ToLower(s1.Sval)}]
			if !ok || ns == "" {
				continue
			}
			loc := int(c.Location)
			end := scanIdent(sql, loc)
			if end > loc {
				edits = append(edits, edit{loc, end, QuotePG(ns)})
			}
		}
	}
	if len(edits) == 0 {
		return sql, nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := sql
	last := len(sql) + 1
	for _, e := range edits {
		if e.end > last { // overlapping spans: refuse rather than corrupt the SQL
			return "", errors.New("overlapping table references")
		}
		out = out[:e.start] + e.text + out[e.end:]
		last = e.start
	}
	return out, nil
}

// ---------------------------------------------------------------- MySQL

var (
	myOnce   sync.Once
	myParser *sqlparser.Parser
)

func vitess() *sqlparser.Parser {
	myOnce.Do(func() {
		p, err := sqlparser.New(sqlparser.Options{MySQLServerVersion: "8.0.40"})
		if err != nil {
			p = sqlparser.NewTestParser()
		}
		myParser = p
	})
	return myParser
}

func parseMy(sql string) (Parsed, error) {
	stmt, err := vitess().Parse(sql)
	if err != nil {
		return Parsed{}, err
	}
	p := Parsed{OK: true}
	refs, aliases := myRefs(stmt)
	p.Refs = refs
	switch n := stmt.(type) {
	case *sqlparser.Select, *sqlparser.Union:
		p.Kind = "select"
		if len(refs) == 0 {
			p.Kind = "other"
		}
	case *sqlparser.Insert:
		p.Kind = "insert"
		if tn, err := n.Table.TableName(); err == nil {
			p.Target = Ref{Schema: tn.Qualifier.String(), Name: tn.Name.String()}
		}
	case *sqlparser.Update:
		p.Kind = "update"
		if len(n.TableExprs) > 0 {
			p.Target = myFirstTable(n.TableExprs[0])
		}
	case *sqlparser.Delete:
		p.Kind = "delete"
		if len(n.Targets) > 0 {
			t := n.Targets[0]
			if r, ok := aliases[strings.ToLower(t.Name.String())]; ok && t.Qualifier.IsEmpty() {
				p.Target = r
			} else {
				p.Target = Ref{Schema: t.Qualifier.String(), Name: t.Name.String()}
			}
		} else if len(n.TableExprs) > 0 {
			p.Target = myFirstTable(n.TableExprs[0])
		}
	default:
		p.Kind = "other"
	}
	p.Fingerprint = myFingerprint(sql)
	return p, nil
}

func myFirstTable(te sqlparser.TableExpr) Ref {
	var out Ref
	_ = sqlparser.Walk(func(n sqlparser.SQLNode) (bool, error) {
		if a, ok := n.(*sqlparser.AliasedTableExpr); ok && out.Name == "" {
			if tn, ok := a.Expr.(sqlparser.TableName); ok {
				out = Ref{Schema: tn.Qualifier.String(), Name: tn.Name.String(), Alias: a.As.String()}
				return false, nil
			}
		}
		return out.Name == "", nil
	}, te)
	return out
}

func myRefs(stmt sqlparser.Statement) ([]Ref, map[string]Ref) {
	ctes := map[string]bool{}
	_ = sqlparser.Walk(func(n sqlparser.SQLNode) (bool, error) {
		if c, ok := n.(*sqlparser.CommonTableExpr); ok {
			ctes[strings.ToLower(c.ID.String())] = true
		}
		return true, nil
	}, stmt)
	var refs []Ref
	aliases := map[string]Ref{}
	_ = sqlparser.Walk(func(n sqlparser.SQLNode) (bool, error) {
		a, ok := n.(*sqlparser.AliasedTableExpr)
		if !ok {
			return true, nil
		}
		tn, ok := a.Expr.(sqlparser.TableName)
		if !ok {
			return true, nil
		}
		name := tn.Name.String()
		if tn.Qualifier.IsEmpty() && (ctes[strings.ToLower(name)] || strings.EqualFold(name, "dual")) {
			return true, nil
		}
		r := Ref{Schema: tn.Qualifier.String(), Name: name, Alias: a.As.String()}
		refs = append(refs, r)
		if r.Alias != "" {
			aliases[strings.ToLower(r.Alias)] = r
		}
		return true, nil
	}, stmt)
	return refs, aliases
}

// myFingerprint replaces literals with placeholders, collapses IN lists and
// multi-row VALUES, drops comments and hashes the canonical text.
func myFingerprint(sql string) string {
	stmt, err := vitess().Parse(sql)
	if err != nil {
		return Fingerprint(sql)
	}
	arg := sqlparser.NewArgument("v")
	norm := sqlparser.Rewrite(stmt, func(c *sqlparser.Cursor) bool {
		switch n := c.Node().(type) {
		case *sqlparser.Literal, *sqlparser.NullVal:
			c.Replace(arg)
		case sqlparser.ValTuple:
			c.Replace(sqlparser.ValTuple{arg})
			return false
		case sqlparser.Values:
			if len(n) > 1 {
				c.Replace(n[:1])
			}
		case *sqlparser.ParsedComments:
			c.Replace((*sqlparser.ParsedComments)(nil))
			return false
		}
		return true
	}, nil)
	h := sha1.Sum([]byte(strings.ToLower(sqlparser.String(norm))))
	return hex.EncodeToString(h[:8])
}

func rewriteMy(sql string, fn func(Ref) (string, string, bool)) (string, error) {
	stmt, err := vitess().Parse(sql)
	if err != nil {
		return "", err
	}
	_, aliases := myRefs(stmt)
	changed := false
	out := sqlparser.Rewrite(stmt, func(c *sqlparser.Cursor) bool {
		tn, ok := c.Node().(sqlparser.TableName)
		if !ok || tn.Name.IsEmpty() {
			return true
		}
		if _, isCol := c.Parent().(*sqlparser.ColName); isCol && tn.Qualifier.IsEmpty() {
			return true // `t.col`: still resolves after the table moves
		}
		if _, isAlias := aliases[strings.ToLower(tn.Name.String())]; isAlias && tn.Qualifier.IsEmpty() {
			if _, isTE := c.Parent().(*sqlparser.AliasedTableExpr); !isTE {
				return true // multi-table DELETE target given by alias
			}
		}
		s, n, ok := fn(Ref{Schema: tn.Qualifier.String(), Name: tn.Name.String()})
		if !ok {
			return true
		}
		c.Replace(sqlparser.NewTableNameWithQualifier(n, s))
		changed = true
		return true
	}, nil)
	if !changed {
		return sql, nil
	}
	return sqlparser.String(out), nil
}

// MySQLAliases maps lowercase aliases (and table names) to table names.
func MySQLAliases(sql string) (map[string]string, bool) {
	stmt, err := vitess().Parse(sql)
	if err != nil {
		return nil, false
	}
	refs, _ := myRefs(stmt)
	out := map[string]string{}
	for _, r := range refs {
		out[strings.ToLower(r.Name)] = r.Name
		if r.Alias != "" {
			out[strings.ToLower(r.Alias)] = r.Name
		}
	}
	return out, true
}

// MySQLWhereProbe turns a single-table UPDATE/DELETE into a SELECT of the rows
// it would touch (MySQL's EXPLAIN ANALYZE doesn't run single-table DML).
func MySQLWhereProbe(sql string) string {
	stmt, err := vitess().Parse(sql)
	if err != nil {
		return WhereProbe(sql)
	}
	var from sqlparser.TableExprs
	var where *sqlparser.Where
	switch n := stmt.(type) {
	case *sqlparser.Update:
		from, where = n.TableExprs, n.Where
	case *sqlparser.Delete:
		from, where = n.TableExprs, n.Where
	default:
		return ""
	}
	if where == nil || len(from) == 0 {
		return ""
	}
	sel := &sqlparser.Select{
		SelectExprs: &sqlparser.SelectExprs{Exprs: []sqlparser.SelectExpr{&sqlparser.AliasedExpr{Expr: sqlparser.NewIntLiteral("1")}}},
		From:        from,
		Where:       where,
	}
	return sqlparser.String(sel)
}
