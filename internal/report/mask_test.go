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

func TestMaskSQLMySQLDoubleQuotesAndHex(t *testing.T) {
	tests := []struct{ name, dialect, in, want string }{
		{"mysql double-quoted string", "mysql", `SELECT * FROM u WHERE email = "bob@x.com" AND id = 7`, `SELECT * FROM u WHERE email = ? AND id = ?`},
		{"mysql hex literal", "mysql", `SELECT * FROM u WHERE tok = 0x414243`, `SELECT * FROM u WHERE tok = ?`},
		{"postgres keeps quoted identifiers", "postgres", `SELECT "Name" FROM "Users" WHERE id = 7`, `SELECT "Name" FROM "Users" WHERE id = ?`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskSQL(tt.in, tt.dialect); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
