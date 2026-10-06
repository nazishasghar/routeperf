// Package auth applies credentials to requests: bearer tokens, custom
// headers, cookies / cookie jars, API keys in query, OAuth2 client
// credentials and login flows; it also maps OpenAPI security schemes.
package auth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

type OAuth2CC struct {
	TokenURL     string `yaml:"token_url"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	Scope        string `yaml:"scope"`
	Audience     string `yaml:"audience"`
}

type Login struct {
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	Body    map[string]any    `yaml:"body"`
	Headers map[string]string `yaml:"headers"`
	Extract struct {
		Cookies    bool   `yaml:"cookies"`
		BearerFrom string `yaml:"bearer_from"`
	} `yaml:"extract"`
	RefreshOn []int `yaml:"refresh_on"`
}

type Config struct {
	Bearer        string            `yaml:"bearer"`
	BearerCommand string            `yaml:"bearer_command,omitempty"` // shell command printing a token (e.g. gcloud auth print-access-token)
	Headers       map[string]string `yaml:"headers"`
	Cookies       map[string]string `yaml:"cookies"`
	CookieJar     string            `yaml:"cookie_jar"`
	Query         map[string]string `yaml:"query"`
	OAuth2        *OAuth2CC         `yaml:"oauth2_client_credentials"`
	Login         *Login            `yaml:"login"`
}

type Manager struct {
	cfg    Config
	base   *url.URL
	Client *http.Client
	jar    *cookiejar.Jar
	mu     sync.Mutex
	token  string // dynamic bearer (oauth2/login)
	exp    time.Time
}

func New(cfg Config, baseURL string, timeout time.Duration) (*Manager, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns, tr.MaxIdleConnsPerHost = 256, 128 // load mode runs many clients at once
	m := &Manager{cfg: cfg, base: base, jar: jar, Client: &http.Client{Jar: jar, Timeout: timeout, Transport: tr}}
	var cs []*http.Cookie
	for k, v := range cfg.Cookies {
		cs = append(cs, &http.Cookie{Name: k, Value: v, Path: "/"})
	}
	if len(cs) > 0 {
		jar.SetCookies(base, cs)
	}
	if cfg.CookieJar != "" {
		if err := m.loadJar(cfg.CookieJar); err != nil {
			return nil, fmt.Errorf("cookie jar: %w", err)
		}
	}
	return m, nil
}

// loadJar reads a Netscape/curl cookie file.
func (m *Manager) loadJar(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var cs []*http.Cookie
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		ln = strings.TrimPrefix(ln, "#HttpOnly_")
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		p := strings.Split(ln, "\t")
		if len(p) < 7 {
			continue
		}
		c := &http.Cookie{Name: p[5], Value: p[6], Path: p[2]}
		if exp, err := strconv.ParseInt(p[4], 10, 64); err == nil && exp > 0 {
			c.Expires = time.Unix(exp, 0)
		}
		cs = append(cs, c)
	}
	m.jar.SetCookies(m.base, cs)
	return sc.Err()
}

func (m *Manager) resolve(p string) string {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	u := *m.base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(p, "/")
	return u.String()
}

// Init performs OAuth2 / login flows and token commands (outside any timed
// section).
func (m *Manager) Init(ctx context.Context) error {
	if m.cfg.BearerCommand != "" {
		if err := m.tokenCommand(ctx); err != nil {
			return fmt.Errorf("bearer_command: %w", err)
		}
	}
	if m.cfg.OAuth2 != nil {
		if err := m.oauth(ctx); err != nil {
			return fmt.Errorf("oauth2 client credentials: %w", err)
		}
	}
	if m.cfg.Login != nil {
		if err := m.login(ctx); err != nil {
			return fmt.Errorf("login: %w", err)
		}
	}
	return nil
}

func (m *Manager) oauth(ctx context.Context) error {
	o := m.cfg.OAuth2
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {o.ClientID}, "client_secret": {o.ClientSecret}}
	if o.Scope != "" {
		form.Set("scope", o.Scope)
	}
	if o.Audience != "" {
		form.Set("audience", o.Audience)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", m.resolve(o.TokenURL), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(o.ClientID, o.ClientSecret)
	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("token endpoint %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var tr struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &tr); err != nil || tr.AccessToken == "" {
		return fmt.Errorf("no access_token in response")
	}
	m.mu.Lock()
	m.token = tr.AccessToken
	if tr.ExpiresIn > 0 {
		m.exp = time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - 30*time.Second)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) login(ctx context.Context) error {
	l := m.cfg.Login
	method := l.Method
	if method == "" {
		method = "POST"
	}
	body, _ := json.Marshal(l.Body)
	req, _ := http.NewRequestWithContext(ctx, method, m.resolve(l.Path), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range l.Headers {
		req.Header.Set(k, v)
	}
	resp, err := m.Client.Do(req) // cookies land in the jar
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("login %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	if l.Extract.BearerFrom != "" {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("login response not JSON: %w", err)
		}
		tok, ok := JSONPath(v, l.Extract.BearerFrom)
		if !ok {
			return fmt.Errorf("bearer_from %s not found in login response", l.Extract.BearerFrom)
		}
		m.mu.Lock()
		m.token = fmt.Sprint(tok)
		m.mu.Unlock()
	}
	return nil
}

// tokenCommand runs bearer_command and uses its trimmed output as the token.
func (m *Manager) tokenCommand(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	sh, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		sh, flag = "cmd", "/C"
	}
	cmd := exec.CommandContext(cctx, sh, flag, m.cfg.BearerCommand)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%v: %s", err, truncate(strings.TrimSpace(stderr.String()), 200))
	}
	tok := strings.TrimSpace(string(out))
	if i := strings.LastIndex(tok, "\n"); i >= 0 { // tools may print notices before the token
		tok = strings.TrimSpace(tok[i+1:])
	}
	if tok == "" {
		return fmt.Errorf("command printed no token")
	}
	m.mu.Lock()
	m.token = tok
	m.mu.Unlock()
	return nil
}

// Refresh re-runs dynamic flows after a 401 (or configured status).
func (m *Manager) Refresh(ctx context.Context, status int) bool {
	if m.cfg.OAuth2 == nil && m.cfg.Login == nil && m.cfg.BearerCommand == "" {
		return false
	}
	if m.cfg.Login != nil && len(m.cfg.Login.RefreshOn) > 0 {
		hit := false
		for _, s := range m.cfg.Login.RefreshOn {
			hit = hit || s == status
		}
		if !hit {
			return false
		}
	} else if status != 401 {
		return false
	}
	return m.Init(ctx) == nil
}

// Bearer returns the current bearer token ("" if none).
func (m *Manager) Bearer() string { return m.bearer() }

// HeaderMap returns the configured custom headers.
func (m *Manager) HeaderMap() map[string]string { return m.cfg.Headers }

// CookieHeader renders the jar's cookies for the API (for non-HTTP transports).
func (m *Manager) CookieHeader() string {
	var parts []string
	for _, c := range m.jar.Cookies(m.base) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func (m *Manager) bearer() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" {
		return m.token
	}
	return m.cfg.Bearer
}

// Apply sets headers, bearer and query credentials (cookies come from the jar).
func (m *Manager) Apply(ctx context.Context, req *http.Request) {
	if m.cfg.OAuth2 != nil && !m.exp.IsZero() && time.Now().After(m.exp) {
		_ = m.oauth(ctx)
	}
	for k, v := range m.cfg.Headers {
		req.Header.Set(k, v)
	}
	if b := m.bearer(); b != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+b)
	}
	if len(m.cfg.Query) > 0 {
		q := req.URL.Query()
		for k, v := range m.cfg.Query {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}
}

func (m *Manager) hasCookie(name string) bool {
	for _, c := range m.jar.Cookies(m.base) {
		if c.Name == name {
			return true
		}
	}
	return false
}

func hasKey(mp map[string]string, k string) bool {
	for x := range mp {
		if strings.EqualFold(x, k) {
			return true
		}
	}
	return false
}

// Satisfies reports whether configured credentials meet an operation's
// security requirements (any alternative suffices).
func (m *Manager) Satisfies(reqs openapi3.SecurityRequirements, schemes map[string]*openapi3.SecurityScheme) (bool, string) {
	if len(reqs) == 0 {
		return true, ""
	}
	var missing []string
	for _, alt := range reqs {
		ok := true
		for name := range alt {
			s := schemes[name]
			if s == nil {
				continue
			}
			sat := false
			switch strings.ToLower(s.Type) {
			case "http":
				if strings.EqualFold(s.Scheme, "basic") {
					sat = hasKey(m.cfg.Headers, "Authorization")
				} else {
					sat = m.bearer() != "" || hasKey(m.cfg.Headers, "Authorization")
				}
			case "apikey":
				switch s.In {
				case "header":
					sat = hasKey(m.cfg.Headers, s.Name) || (strings.EqualFold(s.Name, "Authorization") && m.bearer() != "")
				case "cookie":
					sat = m.hasCookie(s.Name)
				case "query":
					sat = hasKey(m.cfg.Query, s.Name)
				}
			case "oauth2", "openidconnect":
				sat = m.bearer() != ""
			default:
				sat = true
			}
			if !sat {
				ok = false
				missing = append(missing, name)
			}
		}
		if ok {
			return true, ""
		}
	}
	return false, "auth missing for scheme(s): " + strings.Join(dedupe(missing), ", ")
}

// Secrets lists credential values that must never appear in output.
func (m *Manager) Secrets() []string {
	var s []string
	add := func(v string) {
		if len(v) >= 4 {
			s = append(s, v)
		}
	}
	add(m.cfg.Bearer)
	add(m.bearer())
	add(m.token)
	for _, v := range m.cfg.Headers {
		add(v)
	}
	for _, v := range m.cfg.Cookies {
		add(v)
	}
	for _, v := range m.cfg.Query {
		add(v)
	}
	if m.cfg.OAuth2 != nil {
		add(m.cfg.OAuth2.ClientSecret)
	}
	if m.cfg.Login != nil {
		for k, v := range m.cfg.Login.Body {
			if regexp.MustCompile(`(?i)pass|secret|token`).MatchString(k) {
				add(fmt.Sprint(v))
			}
		}
	}
	for _, c := range m.jar.Cookies(m.base) {
		add(c.Value)
	}
	return s
}

func Redact(text string, secrets []string) string {
	for _, s := range secrets {
		text = strings.ReplaceAll(text, s, "***")
	}
	return text
}

var pathTok = regexp.MustCompile(`[^.\[\]]+|\[\d+\]`)

// JSONPath resolves simple paths like $.data.items[0].token.
func JSONPath(v any, path string) (any, bool) {
	path = strings.TrimPrefix(strings.TrimPrefix(path, "$"), ".")
	cur := v
	for _, tok := range pathTok.FindAllString(path, -1) {
		if strings.HasPrefix(tok, "[") {
			i, _ := strconv.Atoi(strings.Trim(tok, "[]"))
			arr, ok := cur.([]any)
			if !ok || i >= len(arr) {
				return nil, false
			}
			cur = arr[i]
			continue
		}
		mp, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mp[tok]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
