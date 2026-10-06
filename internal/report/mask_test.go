package report

import "testing"

func TestMaskSQL(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM users WHERE email = 'a@b.c' AND id = 42 LIMIT 10": "SELECT * FROM users WHERE email = ? AND id = ? LIMIT ?",
		"SELECT * FROM t2 WHERE a = $1 AND b = 'it''s' AND c = -3.5":     "SELECT * FROM t2 WHERE a = $1 AND b = ? AND c = ?",
		"INSERT INTO notes (body) VALUES ($$secret text$$), (E'x\\'y')":  "INSERT INTO notes (body) VALUES (?), (?)",
		"((status)::text = 'paid'::text)":                                "((status)::text = ?::text)",
		"SELECT col1, \"t3\".x FROM t3 WHERE x > 1e3":                    "SELECT col1, \"t3\".x FROM t3 WHERE x > ?",
	}
	for in, want := range cases {
		if got := MaskSQL(in); got != want {
			t.Errorf("MaskSQL(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
