package render

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"text/template/parse"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// These are the renderer's secondary, fast checks. What each variable does
// is proven from outside the process by the integration suite; these guard
// the template and the reference against drift, which nothing outside the
// process can see until a later behaviour breaks.

var update = flag.Bool("update", false, "rewrite the golden files and docs/configuration.md")

// Shapes are environments whose rendering is pinned in testdata/<name>.yaml.
// cmd/otlp-collector-oidc loads every one through the collector's own
// validation.
var shapes = map[string]map[string]string{
	// Only what is required: every other value is its default.
	"minimal": {
		"OIDC_ISSUER_URL":             "https://idp.example.com/application/o/telemetry/",
		"OIDC_AUDIENCE":               "telemetry",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
		"ALLOWED_SERVICE_NAMES":       ".*",
	},
	// Every variable set to something other than its default.
	"full": {
		"OIDC_ISSUER_URL":               "https://idp.example.com/",
		"OIDC_AUDIENCE":                 "aud-1",
		"ALLOWED_SERVICE_NAMES":         "app-.*|integration",
		"OIDC_DISCOVERY_RETRY":          "5s",
		"OIDC_JWKS_REFRESH":             "1h",
		"REQUIRED_SCOPE":                "otlp:send",
		"REQUIRED_CLAIMS":               " sub , email ,",
		"CLOCK_SKEW":                    "0s",
		"REJECTION_LOG_INTERVAL":        "10s",
		"OTEL_EXPORTER_OTLP_ENDPOINT":   "https://upstream.example.com:4317",
		"LISTEN_ADDR":                   "127.0.0.1:14318",
		"TLS_CERT_FILE":                 "/run/tls/tls.crt",
		"TLS_KEY_FILE":                  "/run/tls/tls.key",
		"TLS_RELOAD_INTERVAL":           "10s",
		"MAX_REQUEST_BODY_BYTES":        "1048576",
		"CORS_ALLOWED_ORIGINS":          "https://app.example.com,https://*.example.org",
		"MEMORY_LIMIT_MIB":              "512",
		"MEMORY_SPIKE_LIMIT_MIB":        "128",
		"BATCH_TIMEOUT":                 "200ms",
		"HEALTH_ADDR":                   "0.0.0.0:13134",
		"SELF_METRICS_ADDR":             "127.0.0.1:9888",
		"LOG_LEVEL":                     "debug",
		"LOG_FORMAT":                    "console",
		"LOG_OUTPUT":                    "stdout",
		"ALLOWED_METRIC_NAMES":          `app\.(requests|latency)`,
		"ALLOWED_METRIC_ATTRIBUTE_KEYS": "http.route,user.id,session.id,status",
		"MAX_METRIC_STREAMS":            "500",
		"DELTA_MAX_STALE":               "1m",
		"OTEL_SERVICE_NAME":             "collector-eu",
		"OTEL_RESOURCE_ATTRIBUTES":      "deployment.environment.name=prod,host.name=edge%201",
	},
	// Own logs to the logs upstream alone, following a LOGS override to an
	// HTTP endpoint with its own headers; a service.version in the resource
	// attributes is the build's and is dropped.
	"own-logs-otlp": {
		"OIDC_ISSUER_URL":                  "https://idp.example.com/",
		"OIDC_AUDIENCE":                    "telemetry",
		"ALLOWED_SERVICE_NAMES":            "app-.*|integration",
		"OTEL_EXPORTER_OTLP_ENDPOINT":      "https://tempo.example.com:4317",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://loki.example.com/otlp/v1/logs",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":  "X-Scope-OrgID=tenant-1",
		"LOG_OUTPUT":                       "otlp",
		"OTEL_SERVICE_NAME":                "collector-eu",
		"OTEL_RESOURCE_ATTRIBUTES":         "service.version=9.9.9,service.name=overridden,team=obs",
	},
	// One HTTP upstream for every signal: one exporter, the base endpoint
	// with the signal paths still to append.
	"http": {
		"OIDC_ISSUER_URL":             "https://idp.example.com/",
		"OIDC_AUDIENCE":               "telemetry",
		"ALLOWED_SERVICE_NAMES":       "app-.*|integration",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com/otlp/",
		"OTEL_EXPORTER_OTLP_HEADERS":  "X-Scope-OrgID=tenant-1",
		// Shorter than the collector's first backoff, which is shortened to it.
		"UPSTREAM_RETRY_MAX_ELAPSED": "2s",
	},
	// Traces to a TLS gRPC upstream with mTLS and every base variable set;
	// logs to an HTTP endpoint used verbatim, their headers replacing the
	// base ones; metrics to a third upstream that no pipeline uses yet, so
	// it is not resolved and nothing is rendered for it.
	"per-signal": {
		"OIDC_ISSUER_URL":                       "https://idp.example.com/",
		"OIDC_AUDIENCE":                         "telemetry",
		"ALLOWED_SERVICE_NAMES":                 "app-.*|integration",
		"OTEL_EXPORTER_OTLP_ENDPOINT":           "https://tempo.example.com:4317",
		"OTEL_EXPORTER_OTLP_HEADERS":            "authorization=Bearer%20abc,x-tenant=a",
		"OTEL_EXPORTER_OTLP_TIMEOUT":            "2500",
		"OTEL_EXPORTER_OTLP_COMPRESSION":        "none",
		"OTEL_EXPORTER_OTLP_CERTIFICATE":        "/run/upstream/ca.pem",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": "/run/upstream/client.pem",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY":         "/run/upstream/client-key.pem",
		"UPSTREAM_TLS_INSECURE_SKIP_VERIFY":     "true",
		"UPSTREAM_QUEUE_SIZE":                   "50",
		"UPSTREAM_RETRY_MAX_ELAPSED":            "5m",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":      "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":      "http://loki.example.com:3100/otlp/v1/logs",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":       "x-scope-orgid=b",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":   "http://mimir.example.com:4317",
	},
	// Identity to renamed attributes, and the deployment's attributes on
	// every client resource.
	"identity": {
		"OIDC_ISSUER_URL":             "https://idp.example.com/",
		"OIDC_AUDIENCE":               "telemetry",
		"ALLOWED_SERVICE_NAMES":       "app-.*|integration",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
		"CLAIM_ATTRIBUTES":            "sub=enduser.id, email=enduser.email",
		"CLIENT_RESOURCE_ATTRIBUTES":  "deployment.environment.name=prod,telemetry_source=client",
	},
	// Values that would break naive substitution: quotes, a newline, YAML
	// syntax and ${...} references must arrive as literal strings.
	"hostile": {
		"OIDC_ISSUER_URL":             "https://idp.example.com/?a=${env:HOME}",
		"OIDC_AUDIENCE":               "a\"b\nexporters: {}",
		"ALLOWED_SERVICE_NAMES":       "app-.*|integration",
		"REQUIRED_SCOPE":              "$${x}$",
		"REQUIRED_CLAIMS":             "sub,'quoted',#hash",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
		"OTEL_EXPORTER_OTLP_HEADERS":  "x-a=%22%24%7Benv%3AHOME%7D%20%23%20exporters%3A%20%7B%7D",
		"OTEL_RESOURCE_ATTRIBUTES":    "a\"b${x}#: {}=%24%7Benv%3AHOME%7D",
		"CLAIM_ATTRIBUTES":            "sub=a\"b${x}",
		"CLIENT_RESOURCE_ATTRIBUTES":  "k${y}=v%22${z}",
	},
}

func lookupIn(env map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func TestGolden(t *testing.T) {
	for name, env := range shapes {
		t.Run(name, func(t *testing.T) {
			var s Settings
			require.NoError(t, Load(lookupIn(env), &s), "env: %v", env)
			got, err := Render(s)
			require.NoError(t, err)

			path := filepath.Join("testdata", name+".yaml")
			if *update {
				require.NoError(t, os.WriteFile(path, got, 0o600))
			}
			want, err := os.ReadFile(path) // #nosec G304 -- a fixed test file
			require.NoError(t, err, "run go test ./internal/render -update to create it")
			assert.Equal(t, string(want), string(got), "env: %v", env)
		})
	}
}

// TestTemplateUsesEverySetting is the drift check between the template and
// the variables: every variable in Settings is rendered, and the template
// refers to nothing else.
func TestTemplateUsesEverySetting(t *testing.T) {
	tmpl, err := parseTemplate()
	require.NoError(t, err)
	var used []string
	walk(tmpl.Root, func(ident []string) {
		if len(ident) >= 2 {
			used = append(used, ident[0]+"."+ident[1])
		}
	})

	var declared []string
	resolvedGroups := map[string]bool{}
	for _, g := range groupsOf(reflect.ValueOf(&Settings{}).Elem()) {
		if g.resolver != nil {
			// Its variables are the resolver's to prove; the template must
			// still render the group.
			resolvedGroups[g.name] = true
			assert.True(t, slices.ContainsFunc(used, func(p string) bool { return strings.HasPrefix(p, g.name+".") }),
				"the template never renders .%s", g.name)
			continue
		}
		for _, v := range g.variables {
			declared = append(declared, v.path)
			assert.Contains(t, used, v.path, "%s is declared but the template never renders it", v.name)
		}
	}
	for _, path := range used {
		if group, _, _ := strings.Cut(path, "."); resolvedGroups[group] {
			continue
		}
		assert.Contains(t, declared, path, "the template renders .%s, which is not a variable", path)
	}
}

// walk calls fn with the identifiers of every field reference under node.
func walk(node parse.Node, fn func(ident []string)) {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			walk(c, fn)
		}
	case *parse.ActionNode:
		walk(n.Pipe, fn)
	case *parse.IfNode:
		walkBranch(&n.BranchNode, fn)
	case *parse.WithNode:
		walkRebound(&n.BranchNode, fn)
	case *parse.RangeNode:
		walkRebound(&n.BranchNode, fn)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			walk(c, fn)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			walk(a, fn)
		}
	case *parse.FieldNode:
		fn(n.Ident)
	case *parse.ChainNode:
		walk(n.Node, fn)
	}
}

// walkRebound walks a with or range, whose body sees the pipeline's value as
// dot: fields there are the value's, not Settings', so only the pipeline and
// the else branch are walked.
func walkRebound(n *parse.BranchNode, fn func(ident []string)) {
	walk(n.Pipe, fn)
	walk(n.ElseList, fn)
}

func walkBranch(n *parse.BranchNode, fn func(ident []string)) {
	walk(n.Pipe, fn)
	walk(n.List, fn)
	walk(n.ElseList, fn)
}

// TestReferenceIsCurrent is the drift check between the variables and
// docs/configuration.md, which is generated from their tags.
func TestReferenceIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "configuration.md")
	got := Reference()
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o600))
	}
	want, err := os.ReadFile(path) // #nosec G304 -- a fixed repository file
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "docs/configuration.md is stale: run make docs")
}

// TestReferenceListsEveryVariable guards the generator itself: a variable
// in any group reaches the reference exactly once.
func TestReferenceListsEveryVariable(t *testing.T) {
	ref := string(Reference())
	var names []string
	for _, root := range []any{&Settings{}, &Source{}} {
		for _, g := range groupsOf(reflect.ValueOf(root).Elem()) {
			for _, v := range g.variables {
				names = append(names, v.name)
				assert.Equal(t, 1, strings.Count(ref, "| `"+v.name+"` |"), "%s in the reference", v.name)
			}
		}
	}
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(names))), len(names), "a variable is declared twice: %v", names)
}

// TestMetricsPipelineCarriesNoIdentity is structural, on the rendered YAML
// alone rather than the renderer's internals: in every golden, no processor
// of the metrics pipeline is an identity processor or refers to an auth.
// key. Behaviour proves the metrics arrive clean today; this guards the
// pipeline's shape against a later edit (the AGENTS.md trust boundary).
func TestMetricsPipelineCarriesNoIdentity(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file) // #nosec G304 -- the package's own goldens
			require.NoError(t, err)
			var cfg struct {
				Processors map[string]any `yaml:"processors"`
				Service    struct {
					Pipelines map[string]struct {
						Processors []string `yaml:"processors"`
					} `yaml:"pipelines"`
				} `yaml:"service"`
			}
			require.NoError(t, yaml.Unmarshal(raw, &cfg))
			metrics, ok := cfg.Service.Pipelines["metrics"]
			require.True(t, ok, "no metrics pipeline")
			require.NotEmpty(t, metrics.Processors)
			for _, name := range metrics.Processors {
				assert.NotContains(t, name, "identity", "the metrics pipeline uses %s", name)
				body, err := yaml.Marshal(cfg.Processors[name])
				require.NoError(t, err)
				assert.NotContains(t, string(body), "auth.", "processor %s of the metrics pipeline refers to the auth context:\n%s", name, body)
			}
			for _, signal := range []string{"traces", "logs"} {
				assert.Contains(t, cfg.Service.Pipelines[signal].Processors, "attributes/identity", "%s lost its identity", signal)
			}
		})
	}
}
