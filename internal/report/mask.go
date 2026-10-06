package report

import (
	"regexp"
	"strings"

	"github.com/nazishasghar/routeperf/internal/plan"
	"github.com/nazishasghar/routeperf/internal/runner"
)

var (
	reStr    = regexp.MustCompile(`(?s)[eEbBxXnN]?'(?:[^'\\]|''|\\.)*'`)
	reDollar = regexp.MustCompile(`(?s)\$([A-Za-z_]*)\$.*?\$([A-Za-z_]*)\$`)
	reNum    = regexp.MustCompile(`(^|[^\w$."])-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b`)
)

// MaskSQL replaces literal values (strings, numbers, dollar-quoted text)
// with ? while keeping the statement's shape and formatting.
func MaskSQL(s string) string {
	if s == "" {
		return s
	}
	s = reDollar.ReplaceAllString(s, "?")
	s = reStr.ReplaceAllString(s, "?")
	var b strings.Builder
	last := 0
	for _, m := range reNum.FindAllStringSubmatchIndex(s, -1) { // $1 and t2 are not literals
		b.WriteString(s[last:m[3]])
		b.WriteString("?")
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func maskPlan(p *plan.Plan) {
	if p == nil || p.Root == nil {
		return
	}
	p.Walk(func(n *plan.Node, _ int) {
		n.Filter, n.IndexCond, n.SortKey = MaskSQL(n.Filter), MaskSQL(n.IndexCond), MaskSQL(n.SortKey)
		if p.Dialect == "mysql" {
			n.RawType = MaskSQL(n.RawType)
		} else {
			n.RawType = reStr.ReplaceAllString(n.RawType, "?")
		}
	})
	plan.Finish(p)
}

// MaskLiterals strips SQL literals and bind parameters from a result, so a
// report can be shared without leaking user data.
func MaskLiterals(res *runner.Result) {
	if res.Masked {
		return
	}
	res.Masked = true
	for _, o := range res.Ops {
		for _, q := range o.Queries {
			q.SQL, q.Example.SQL, q.Example.Params = MaskSQL(q.SQL), MaskSQL(q.Example.SQL), nil
			maskPlan(q.Base)
			for i := range q.Scale {
				maskPlan(q.Scale[i].Plan)
			}
			for i, n := range q.Notes {
				q.Notes[i] = MaskSQL(n)
			}
			if q.Error != "" {
				q.Error = MaskSQL(q.Error)
			}
		}
		for i := range o.Advice {
			o.Advice[i].Message = maskMessage(o.Advice[i].Message)
		}
		for i, n := range o.Notes {
			o.Notes[i] = MaskSQL(n)
		}
	}
	for i, w := range res.Warnings {
		res.Warnings[i] = MaskSQL(w)
	}
}

// maskMessage masks quoted values in advice text but keeps the numbers that
// describe the problem (rows examined, loops).
func maskMessage(s string) string { return reStr.ReplaceAllString(s, "?") }
