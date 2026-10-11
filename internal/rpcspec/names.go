package rpcspec

import (
	"regexp"
	"strings"
)

var (
	readVerb = regexp.MustCompile(`^(?i:get|list|search|find|read|fetch|query|lookup|count|describe|show|view|check|stat|load|has|is|exists)([A-Z_]|$)`)
	anyVerb  = regexp.MustCompile(`^(?i:get|list|search|find|read|fetch|query|lookup|count|describe|show|view|check|stat|load|create|update|delete|remove|add|set|put|patch|upsert|insert|edit|save|batch|all|by|recent|latest|infinite)([A-Z_]|$)`)
	bySuffix = regexp.MustCompile(`(All|By[A-Z]\w*|For[A-Z]\w*|With[A-Z]\w*)$`)
	camel    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

// split breaks a procedure name into its namespace and final segment:
// user.byId, admin/users.list, users_get, getUser.
func split(name string) ([]string, string) {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '/' || r == ':' })
	if len(parts) == 1 && strings.Contains(parts[0], "_") && !strings.HasPrefix(parts[0], "_") {
		parts = strings.Split(parts[0], "_")
	}
	if len(parts) == 0 {
		return nil, name
	}
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// IsRead reports whether a procedure name reads (getUser, user.list,
// users_search): its final segment starts with a read verb.
func IsRead(name string) bool {
	_, last := split(name)
	return readVerb.MatchString(last)
}

// Resource names the table a procedure most likely works on, as a snake
// case noun: user.byId → user, orderItems.list → order_items, getUser →
// user, users_get → users.
func Resource(name string) string {
	ns, last := split(name)
	for i := len(ns) - 1; i >= 0; i-- {
		if ns[i] != "" && !anyVerb.MatchString(ns[i]) {
			return snake(ns[i])
		}
	}
	noun := last
	for noun != "" {
		m := anyVerb.FindStringSubmatchIndex(noun)
		if m == nil {
			break
		}
		noun = strings.TrimLeft(noun[m[2]:], "_") // m[2]: where the verb ends
	}
	noun = bySuffix.ReplaceAllString(noun, "")
	if noun == "" {
		return snake(last)
	}
	return snake(noun)
}

func snake(s string) string {
	return strings.ToLower(camel.ReplaceAllString(s, "${1}_${2}"))
}
