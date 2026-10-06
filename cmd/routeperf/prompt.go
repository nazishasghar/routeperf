package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/nazishasghar/routeperf/internal/auth"
)

type authLogin = auth.Login
type authOAuth = auth.OAuth2CC
type authCreds = auth.Creds
type authRole = auth.Role

type prompter struct{ in *bufio.Reader }

func newPrompter() *prompter { return &prompter{in: bufio.NewReader(os.Stdin)} }

func isTTY(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func (p *prompter) ask(label, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	s, _ := p.in.ReadString('\n')
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	return s
}

func (p *prompter) secret(label string) string {
	fmt.Fprintf(os.Stderr, "%s (hidden): ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		s, _ := p.in.ReadString('\n')
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(b))
}

func (p *prompter) yes(label string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	s := strings.ToLower(p.ask(label+" ["+d+"]", ""))
	if s == "" {
		return def
	}
	return s == "y" || s == "yes"
}

func (p *prompter) choose(label, def string, n int) []int {
	s := p.ask(label, def)
	var out []int
	for _, part := range strings.Split(s, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && v >= 1 && v <= n {
			out = append(out, v)
		}
	}
	return out
}
