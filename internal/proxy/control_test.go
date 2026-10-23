package proxy

import "testing"

func TestWindowClampsToRetainedRecords(t *testing.T) {
	p := &Proxy{base: 100, recs: []record{{sql: "a"}, {sql: "b"}, {sql: "c"}}}
	tests := []struct {
		name     string
		from, to int64
		want     int
	}{
		{"inside", 100, 102, 2},
		{"starts before the trimmed history", 90, 101, 1},
		{"entirely trimmed away", 10, 20, 0},
		{"past the end", 101, 500, 2},
		{"inverted", 102, 100, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(p.window(tt.from, tt.to)); got != tt.want {
				t.Fatalf("window(%d,%d) has %d records, want %d", tt.from, tt.to, got, tt.want)
			}
		})
	}
}
