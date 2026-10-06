package runner

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nazishasghar/routeperf/internal/auth"
)

type Config struct {
	Spec string `yaml:"spec"`
	API  struct {
		BaseURL  string `yaml:"base_url"`
		Timeout  string `yaml:"timeout"`
		Protocol string `yaml:"protocol,omitempty"` // openapi | graphql | grpc (auto-detected from spec)
		GraphQL  string `yaml:"graphql_path,omitempty"`
		GRPC     struct {
			Target      string   `yaml:"target,omitempty"` // host:port
			TLS         bool     `yaml:"tls,omitempty"`
			ImportPaths []string `yaml:"import_paths,omitempty"`
		} `yaml:"grpc,omitempty"`
	} `yaml:"api"`
	DB struct {
		URL         string   `yaml:"url"`
		AllowRemote bool     `yaml:"allow_remote"`
		Schemas     []string `yaml:"schemas,omitempty"` // MySQL: extra databases the app reads
	} `yaml:"db"`
	Auth    auth.Config `yaml:"auth"`
	Capture struct {
		Mode        string `yaml:"mode,omitempty"` // log (default) | proxy
		ProxyListen string `yaml:"proxy_listen,omitempty"`
		PGLogFile   string `yaml:"pg_log_file"`
		PGPlanMode  string `yaml:"pg_plan_mode"` // auto | custom | generic
		Settle      string `yaml:"settle"`
		Traceparent *bool  `yaml:"traceparent,omitempty"` // send W3C traceparent; attribute sqlcommenter-tagged SQL
	} `yaml:"capture"`
	Run struct {
		Methods      []string `yaml:"methods"`
		Warmup       int      `yaml:"warmup"`
		Iterations   int      `yaml:"iterations"`
		KIterations  int      `yaml:"k_iterations"`
		Include      []string `yaml:"include"`
		Exclude      []string `yaml:"exclude"`
		DangerousOps []string `yaml:"dangerous_ops"`
	} `yaml:"run"`
	Scale struct {
		Disabled   bool      `yaml:"disabled"`
		DataSteps  []float64 `yaml:"data_steps"`
		KParams    []string  `yaml:"k_params"`
		KSteps     []int     `yaml:"k_steps"`
		Repeats    int       `yaml:"repeats"`
		MaxRepeats int       `yaml:"max_repeats"`
		CITarget   float64   `yaml:"ci_target"` // repeat until the slope's 95% CI half-width is at most this
		PerTable   *bool     `yaml:"per_table,omitempty"`
		Keep       bool      `yaml:"keep"`
	} `yaml:"scale"`
	Cache struct {
		Cold        bool   `yaml:"cold"`               // also measure first hits after cache eviction
		ColdCmd     string `yaml:"cold_cmd,omitempty"` // shell command run before each cold sample (e.g. drop OS caches)
		ColdSamples int    `yaml:"cold_samples"`
	} `yaml:"cache"`
	Advice struct {
		Verify *bool `yaml:"verify,omitempty"` // prove index advice with HypoPG when available
	} `yaml:"advice"`
	Writes struct {
		Enabled  *bool  `yaml:"enabled"`
		Snapshot string `yaml:"snapshot"` // tables | none
	} `yaml:"writes"`
	Load struct {
		Enabled     bool   `yaml:"enabled"`
		Concurrency []int  `yaml:"concurrency"`
		Duration    string `yaml:"duration"`
		Writes      bool   `yaml:"writes"`
	} `yaml:"load"`
	Thresholds struct {
		P95Ms      float64 `yaml:"p95_ms"`
		MaxQueries float64 `yaml:"max_queries_per_request"`
		MaxDegree  float64 `yaml:"max_degree"`
	} `yaml:"thresholds"`
	Report struct {
		MaskLiterals bool  `yaml:"mask_literals"`
		HTML         *bool `yaml:"html,omitempty"`
	} `yaml:"report"`
	Fixtures string `yaml:"fixtures"`
	Out      string `yaml:"out"`
	Yes      bool   `yaml:"-"`
	Verbose  bool   `yaml:"-"`
}

func (c *Config) PerTable() bool   { return c.Scale.PerTable == nil || *c.Scale.PerTable }
func (c *Config) Verify() bool     { return c.Advice.Verify == nil || *c.Advice.Verify }
func (c *Config) TraceOn() bool    { return c.Capture.Traceparent == nil || *c.Capture.Traceparent }
func (c *Config) HTMLReport() bool { return c.Report.HTML == nil || *c.Report.HTML }
func (c *Config) Proxy() bool      { return c.Capture.Mode == "proxy" }

func (c *Config) loadDuration() time.Duration {
	d, err := time.ParseDuration(c.Load.Duration)
	if err != nil || d <= 0 {
		return 5 * time.Second
	}
	return d
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// LoadConfig reads YAML with ${VAR} / ${VAR:-default} interpolation.
func LoadConfig(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
		} else {
			s := envRe.ReplaceAllStringFunc(string(b), func(m string) string {
				g := envRe.FindStringSubmatch(m)
				if v, ok := os.LookupEnv(g[1]); ok {
					return v
				}
				return g[2]
			})
			if err := yaml.Unmarshal([]byte(s), c); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return c, nil
}

func (c *Config) Defaults() {
	if c.API.Timeout == "" {
		c.API.Timeout = "30s"
	}
	if c.Capture.PGPlanMode == "" {
		c.Capture.PGPlanMode = "auto"
	}
	if c.Capture.Settle == "" {
		c.Capture.Settle = "40ms"
	}
	if len(c.Run.Methods) == 0 {
		c.Run.Methods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	}
	if c.Run.Warmup == 0 {
		c.Run.Warmup = 2
	}
	if c.Run.Iterations == 0 {
		c.Run.Iterations = 10
	}
	if c.Run.KIterations == 0 {
		c.Run.KIterations = 3
	}
	if len(c.Scale.DataSteps) == 0 {
		c.Scale.DataSteps = []float64{0.01, 0.03, 0.10, 0.30, 1.0}
	}
	if len(c.Scale.KParams) == 0 {
		c.Scale.KParams = []string{"limit", "page_size", "pageSize", "per_page", "perPage", "size", "top", "first", "count", "take"}
	}
	if len(c.Scale.KSteps) == 0 {
		c.Scale.KSteps = []int{1, 10, 100, 250, 500, 1000}
	}
	if c.Scale.Repeats == 0 {
		c.Scale.Repeats = 3
	}
	if c.Scale.MaxRepeats < c.Scale.Repeats {
		c.Scale.MaxRepeats = 12
		if c.Scale.MaxRepeats < c.Scale.Repeats {
			c.Scale.MaxRepeats = c.Scale.Repeats
		}
	}
	if c.Scale.CITarget == 0 {
		c.Scale.CITarget = 0.1
	}
	if c.Cache.ColdSamples == 0 {
		c.Cache.ColdSamples = 3
	}
	if len(c.Load.Concurrency) == 0 {
		c.Load.Concurrency = []int{1, 4, 16, 32}
	}
	if c.Load.Duration == "" {
		c.Load.Duration = "5s"
	}
	if c.Capture.Mode == "" {
		c.Capture.Mode = "log"
	}
	if c.Capture.ProxyListen == "" {
		c.Capture.ProxyListen = "127.0.0.1:6543"
	}
	if c.API.GraphQL == "" {
		c.API.GraphQL = "/graphql"
	}
	if c.Writes.Snapshot == "" {
		c.Writes.Snapshot = "tables"
	}
	if c.Out == "" {
		c.Out = "routeperf-out"
	}
	if c.Fixtures == "" {
		c.Fixtures = "fixtures.yaml"
	}
	c.Run.Methods = upper(c.Run.Methods)
}

func (c *Config) WritesEnabled() bool {
	if c.Writes.Enabled != nil && !*c.Writes.Enabled {
		return false
	}
	for _, m := range c.Run.Methods {
		if m != "GET" && m != "HEAD" {
			return true
		}
	}
	return false
}

func (c *Config) timeout() time.Duration {
	d, err := time.ParseDuration(c.API.Timeout)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

func (c *Config) settle() time.Duration {
	d, err := time.ParseDuration(c.Capture.Settle)
	if err != nil {
		return 40 * time.Millisecond
	}
	return d
}

func upper(s []string) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = strings.ToUpper(strings.TrimSpace(x))
	}
	return out
}

const SampleConfig = `# routeperf configuration. Values support ${ENV_VAR} and ${ENV_VAR:-default}.
spec: http://localhost:3000/swagger.json   # OpenAPI/Swagger URL or file; or a GraphQL endpoint / .graphql SDL; or grpc://host:port / .proto files
api:
  base_url: http://localhost:3000
  timeout: 30s
  # protocol: openapi            # openapi | graphql | grpc (auto-detected from spec)
  # graphql_path: /graphql       # GraphQL: endpoint path when spec is an SDL file
  # grpc: { target: localhost:50051, tls: false, import_paths: [./proto] }
db:
  url: ${DATABASE_URL}          # postgres://user@localhost:5432/app  or  mysql://root@127.0.0.1:3306/app
  allow_remote: false           # never point at production
  # schemas: [reporting]        # MySQL: other databases the app reads (Postgres sees every schema)

auth:                           # any combination
  bearer: ${API_TOKEN:-}
  # bearer_command: gcloud auth print-access-token   # run once (and again on 401) to get the token
  headers: {}                   # e.g. { X-Tenant-Id: acme, X-Api-Key: "${API_KEY}" }
  cookies: {}                   # e.g. { session: "${SESSION_COOKIE}" }
  # cookie_jar: ./cookies.txt   # Netscape/curl format
  # query: { api_key: "${API_KEY}" }
  # oauth2_client_credentials:
  #   token_url: http://localhost:3000/oauth/token
  #   client_id: ${CLIENT_ID}
  #   client_secret: ${CLIENT_SECRET}
  #   scope: "read write"
  # login:
  #   method: POST
  #   path: /auth/login
  #   body: { email: perf@test.dev, password: "${PERF_PASSWORD}" }
  #   extract: { cookies: true, bearer_from: "$.data.accessToken" }
  #   refresh_on: [401]

capture:
  mode: log                     # log: server statement log (needs admin) | proxy: wire proxy, point the app at proxy_listen
  # proxy_listen: 127.0.0.1:6543
  # pg_log_file: /opt/homebrew/var/log/postgresql@18.log   # auto-detected when omitted (or docker:<container>)
  pg_plan_mode: auto            # auto: use a generic plan when the app's prepared statements do | custom | generic
  settle: 40ms
  traceparent: true             # send W3C traceparent; SQL tagged by sqlcommenter is attributed by trace id

run:
  methods: [GET, HEAD, POST, PUT, PATCH, DELETE]
  warmup: 2
  iterations: 10
  k_iterations: 3
  include: []                   # operationIds or tag:<name>
  exclude: []
  dangerous_ops: []             # collection deletes / reset endpoints must be listed to run

scale:
  data_steps: [0.01, 0.03, 0.10, 0.30, 1.0]
  k_steps: [1, 10, 100, 250, 500, 1000]
  repeats: 3                    # minimum repeats per point
  max_repeats: 12               # keep repeating until the slope's 95% CI is within ci_target
  ci_target: 0.1
  per_table: true               # also shrink one table at a time to attribute growth

cache:
  cold: false                   # also measure first hits after evicting the tables from the DB cache
  # cold_cmd: "sync && sudo purge"   # optional: drop OS caches too before each cold sample
  cold_samples: 3

advice:
  verify: true                  # prove index advice with HypoPG (Postgres) before reporting it

load:
  enabled: false                # concurrent load per endpoint (lock contention, pool limits)
  concurrency: [1, 4, 16, 32]
  duration: 5s
  writes: false

writes:
  snapshot: tables              # tables | none

thresholds:
  p95_ms: 300
  max_queries_per_request: 10

report:
  mask_literals: false          # replace SQL literals/parameters with ? in every report
  html: true                    # also write report.html

fixtures: fixtures.yaml
out: routeperf-out
`
