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
		BaseURL string `yaml:"base_url"`
		Timeout string `yaml:"timeout"`
	} `yaml:"api"`
	DB struct {
		URL         string `yaml:"url"`
		AllowRemote bool   `yaml:"allow_remote"`
	} `yaml:"db"`
	Auth    auth.Config `yaml:"auth"`
	Capture struct {
		PGLogFile  string `yaml:"pg_log_file"`
		PGPlanMode string `yaml:"pg_plan_mode"` // auto | custom | generic
		Settle     string `yaml:"settle"`
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
		Disabled  bool      `yaml:"disabled"`
		DataSteps []float64 `yaml:"data_steps"`
		KParams   []string  `yaml:"k_params"`
		KSteps    []int     `yaml:"k_steps"`
		Repeats   int       `yaml:"repeats"`
		Keep      bool      `yaml:"keep"`
	} `yaml:"scale"`
	Writes struct {
		Enabled  *bool  `yaml:"enabled"`
		Snapshot string `yaml:"snapshot"` // tables | none
	} `yaml:"writes"`
	Thresholds struct {
		P95Ms      float64 `yaml:"p95_ms"`
		MaxQueries float64 `yaml:"max_queries_per_request"`
		MaxDegree  float64 `yaml:"max_degree"`
	} `yaml:"thresholds"`
	Fixtures string `yaml:"fixtures"`
	Out      string `yaml:"out"`
	Yes      bool   `yaml:"-"`
	Verbose  bool   `yaml:"-"`
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
spec: http://localhost:3000/swagger.json
api:
  base_url: http://localhost:3000
  timeout: 30s
db:
  url: ${DATABASE_URL}          # postgres://user@localhost:5432/app  or  mysql://root@127.0.0.1:3306/app
  allow_remote: false           # never point at production

auth:                           # any combination
  bearer: ${API_TOKEN:-}
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
  # pg_log_file: /opt/homebrew/var/log/postgresql@18.log   # auto-detected when omitted (or docker:<container>)
  pg_plan_mode: auto            # auto: use a generic plan when the app's prepared statements do | custom | generic
  settle: 40ms

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
  repeats: 3

writes:
  snapshot: tables              # tables | none

thresholds:
  p95_ms: 300
  max_queries_per_request: 10

fixtures: fixtures.yaml
out: routeperf-out
`
