package db

import "testing"

func TestHasPrefixFold(t *testing.T) {
	for _, msg := range []string{"parameters: $1 = '42'", "Parameters: $1 = '42'"} {
		if !hasPrefixFold(msg, "parameters:") {
			t.Errorf("%q should match", msg)
		}
	}
	if hasPrefixFold("param", "parameters:") || hasPrefixFold("duration: 3 ms", "parameters:") {
		t.Error("unrelated detail matched")
	}
}
