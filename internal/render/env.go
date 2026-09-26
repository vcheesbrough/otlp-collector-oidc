package render

import (
	"encoding"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Lookup reads one environment variable; os.LookupEnv in the binary.
type Lookup func(name string) (string, bool)

// Settings is every group the template renders, and is the template's whole
// view: nothing else in the environment reaches the YAML.
type Settings struct {
	Identity  IdentitySettings `group:"Identity provider"`
	Upstream  UpstreamSettings `group:"Upstream"`
	Listener  ListenerSettings `group:"Listener and TLS"`
	Resources ResourceSettings `group:"Resources and batching"`
	Self      SelfSettings     `group:"Health and own metrics"`
	Logs      LogSettings      `group:"Own logs"`
}

// Source is where the configuration comes from. The run command reads it;
// the template never sees it.
type Source struct {
	Config SourceSettings `group:"Replacing the pipeline"`
}

// Field tags, read by Load and Reference:
//
//	env      the variable's name
//	required "true" when the process must not start without it
//	default  the value used when the variable is unset or empty
//	doc      one line for the reference; 'x' is set as code
//
// A variable set to the empty string counts as unset.

// IdentitySettings configures the authenticator (oidcclientauth).
type IdentitySettings struct {
	IssuerURL            string        `env:"OIDC_ISSUER_URL"        required:"true"                    doc:"Exact 'iss' value; discovery reads '<issuer>/.well-known/openid-configuration'"`
	Audience             string        `env:"OIDC_AUDIENCE"          required:"true"                    doc:"Value 'aud' must contain, normally the provider client id"`
	DiscoveryRetry       time.Duration `env:"OIDC_DISCOVERY_RETRY"   default:"30s"                      doc:"Retry interval while discovery fails; requests get '503' meanwhile"`
	JWKSRefresh          time.Duration `env:"OIDC_JWKS_REFRESH"      default:"10m"                      doc:"JWKS re-read interval: the longest a key the provider removes is still trusted"`
	RequiredScope        string        `env:"REQUIRED_SCOPE"         default:"telemetry:write"          doc:"Must appear in 'scope' (or 'scp')"`
	RequiredClaims       []string      `env:"REQUIRED_CLAIMS"        default:"sub,preferred_username"   doc:"Comma-separated claims that must each be present and non-empty"`
	ClockSkew            time.Duration `env:"CLOCK_SKEW"             default:"60s"                      doc:"Tolerance on 'exp', 'nbf' and 'iat'"`
	RejectionLogInterval time.Duration `env:"REJECTION_LOG_INTERVAL" default:"60s"                      doc:"At most one warning per refusal reason per interval"`
}

// ListenerSettings configures the one OTLP listener.
type ListenerSettings struct {
	ListenAddr          Addr          `env:"LISTEN_ADDR"            default:"0.0.0.0:4318"                          doc:"The one listener: TLS, OTLP/gRPC and OTLP/HTTP on the same port"`
	TLSCertFile         string        `env:"TLS_CERT_FILE"          default:"/etc/otlp-collector-oidc/tls/cert.pem" doc:"Server certificate (PEM); the image's embedded self-signed one unless set"`
	TLSKeyFile          string        `env:"TLS_KEY_FILE"           default:"/etc/otlp-collector-oidc/tls/key.pem"  doc:"Its private key (PEM); set together with 'TLS_CERT_FILE'"`
	TLSReloadInterval   time.Duration `env:"TLS_RELOAD_INTERVAL"    default:"1m"                                    doc:"How often the pair is re-read from disk, so a rotated certificate needs no restart"`
	MaxRequestBodyBytes int64         `env:"MAX_REQUEST_BODY_BYTES" default:"4194304"                               doc:"Cap on the decompressed request, '413' beyond it; a gRPC message may be 5 bytes less (its frame header), 'ResourceExhausted' beyond it"`
	CORSAllowedOrigins  []string      `env:"CORS_ALLOWED_ORIGINS"                                                   doc:"Comma-separated origins allowed to send from a browser on another origin; empty turns CORS off"`
}

// ResourceSettings bounds memory and batching.
type ResourceSettings struct {
	MemoryLimitMiB      uint32        `env:"MEMORY_LIMIT_MIB"       default:"64"  doc:"'memory_limiter' hard limit"`
	MemorySpikeLimitMiB uint32        `env:"MEMORY_SPIKE_LIMIT_MIB" default:"16"  doc:"'memory_limiter' spike limit; less than 'MEMORY_LIMIT_MIB'"`
	BatchTimeout        time.Duration `env:"BATCH_TIMEOUT"          default:"5s"  doc:"Longest a record waits in the batcher"`
}

// SelfSettings places the health endpoint and the own-metrics endpoint.
type SelfSettings struct {
	HealthAddr      Addr `env:"HEALTH_ADDR"       default:"127.0.0.1:13133" doc:"Liveness endpoint; never checks the upstream or the provider"`
	SelfMetricsAddr Addr `env:"SELF_METRICS_ADDR" default:"0.0.0.0:8888"    doc:"Prometheus endpoint for the collector's own metrics, at '/metrics'"`
}

// LogSettings configures the process's own log lines.
type LogSettings struct {
	Level  LogLevel  `env:"LOG_LEVEL"  default:"info" doc:"'debug', 'info', 'warn' or 'error'; at 'debug' the rendered configuration is logged"`
	Format LogFormat `env:"LOG_FORMAT" default:"json" doc:"Stdout encoding, 'json' or 'console'"`
}

// SourceSettings chooses between the shipped pipeline and a mounted one.
type SourceSettings struct {
	CollectorConfig string `env:"COLLECTOR_CONFIG" doc:"Path of a collector configuration to run instead of the shipped pipeline. Nothing is rendered: every other variable is ignored unless that file reads it with '${env:...}', except 'LOG_LEVEL' and 'LOG_FORMAT', which still govern the run command's own lines. The custom components remain available to it"`
}

// Validate refuses a certificate without its key, or the reverse: half a
// mounted pair would be matched with half the embedded one.
func (s ListenerSettings) Validate() error {
	certDefault := s.TLSCertFile == defaultOf[ListenerSettings]("TLSCertFile")
	keyDefault := s.TLSKeyFile == defaultOf[ListenerSettings]("TLSKeyFile")
	if certDefault != keyDefault {
		return errors.New("TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}
	return nil
}

// Validate refuses a spike limit the hard limit cannot accommodate.
func (s ResourceSettings) Validate() error {
	if s.MemorySpikeLimitMiB >= s.MemoryLimitMiB {
		return fmt.Errorf("MEMORY_SPIKE_LIMIT_MIB (%d) must be less than MEMORY_LIMIT_MIB (%d)", s.MemorySpikeLimitMiB, s.MemoryLimitMiB)
	}
	return nil
}

// defaultOf is the default tag of field in group T.
func defaultOf[T any](field string) string {
	f, ok := reflect.TypeFor[T]().FieldByName(field)
	if !ok {
		panic("render: no field " + field) // programming error, caught by any test
	}
	return f.Tag.Get("default")
}

// ErrRequired marks a required variable that is unset or empty.
var ErrRequired = errors.New("is required")

// VariableError is a variable that is missing or whose value is malformed.
type VariableError struct {
	Name  string
	Value string
	Err   error
}

func (e *VariableError) Error() string {
	if errors.Is(e.Err, ErrRequired) {
		return e.Name + " " + e.Err.Error()
	}
	return fmt.Sprintf("invalid %s %q: %v", e.Name, e.Value, e.Err)
}

func (e *VariableError) Unwrap() error { return e.Err }

// validator is a group with a rule across its fields.
type validator interface {
	Validate() error
}

// Load fills dst, a pointer to a struct of groups, from lookup. It reports
// every missing or malformed variable, not just the first.
func Load(lookup Lookup, dst any) error {
	root := reflect.ValueOf(dst)
	if root.Kind() != reflect.Pointer || root.Elem().Kind() != reflect.Struct {
		panic("render: Load needs a pointer to a struct of groups") // programming error
	}
	var errs []error
	for _, g := range groupsOf(root.Elem()) {
		var groupErrs []error
		if g.resolver != nil {
			if err := g.resolver.resolve(lookup); err != nil {
				groupErrs = append(groupErrs, err)
			}
		}
		for _, v := range g.variables {
			if !v.field.IsValid() {
				continue // the resolver's
			}
			if err := v.load(lookup); err != nil {
				groupErrs = append(groupErrs, err)
			}
		}
		// A group's own rule only means something once its fields parsed.
		if val, ok := g.value.Interface().(validator); ok && len(groupErrs) == 0 {
			if err := val.Validate(); err != nil {
				groupErrs = append(groupErrs, err)
			}
		}
		errs = append(errs, groupErrs...)
	}
	return errors.Join(errs...)
}

// group is one group struct and its variables, in declaration order.
type group struct {
	title     string
	name      string // the field's name in its root, as the template refers to it
	value     reflect.Value
	variables []variable
	// resolver, when set, reads the group's variables itself: they have no
	// field of their own, and the template reaches the group through its
	// methods.
	resolver resolvedGroup
	// note follows the group's table in the reference.
	note string
}

// variable is one tagged field, or one variable of a resolved group.
type variable struct {
	name     string
	required bool
	def      string
	doc      string
	field    reflect.Value // zero for a resolved group's variable
	path     string        // Group.Field, as the template refers to it
}

// resolvedGroup is a group whose variables mean more than one value each, so
// another package reads and documents them rather than the tags.
type resolvedGroup interface {
	resolve(lookup Lookup) error
	reference() (note string, vars []variable)
}

func groupsOf(root reflect.Value) []group {
	var out []group
	for i := range root.NumField() {
		sf := root.Type().Field(i)
		title, ok := sf.Tag.Lookup("group")
		if !ok {
			continue
		}
		g := group{title: title, name: sf.Name, value: root.Field(i)}
		if r, ok := g.value.Addr().Interface().(resolvedGroup); ok {
			g.resolver = r
			g.note, g.variables = r.reference()
			out = append(out, g)
			continue
		}
		for j := range g.value.NumField() {
			vf := g.value.Type().Field(j)
			name, ok := vf.Tag.Lookup("env")
			if !ok {
				continue
			}
			g.variables = append(g.variables, variable{
				name:     name,
				required: vf.Tag.Get("required") == "true",
				def:      vf.Tag.Get("default"),
				doc:      vf.Tag.Get("doc"),
				field:    g.value.Field(j),
				path:     sf.Name + "." + vf.Name,
			})
		}
		out = append(out, g)
	}
	return out
}

func (v variable) load(lookup Lookup) error {
	raw, ok := lookup(v.name)
	if !ok || raw == "" {
		if v.required {
			return &VariableError{Name: v.name, Err: ErrRequired}
		}
		raw = v.def
	}
	if err := parseInto(v.field, raw); err != nil {
		return &VariableError{Name: v.name, Value: redacted(raw), Err: err}
	}
	return nil
}

var (
	durationType        = reflect.TypeFor[time.Duration]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// parseInto sets field from raw according to the field's type.
func parseInto(field reflect.Value, raw string) error {
	if field.Addr().Type().Implements(textUnmarshalerType) {
		//nolint:forcetypeassert // checked on the line above
		return field.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(raw))
	}
	if field.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return errors.New("must be a duration such as 30s or 5m")
		}
		field.SetInt(int64(d))
		return nil
	}
	//exhaustive:ignore // only the kinds a variable may have; the default panics
	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Slice:
		field.Set(reflect.ValueOf(splitList(raw)))
	case reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return errors.New("must be a whole number")
		}
		field.SetInt(n)
	case reflect.Uint32:
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return errors.New("must be a whole number of MiB, 0 to 4294967295")
		}
		field.SetUint(n)
	default:
		panic("render: no parser for " + field.Type().String()) // programming error
	}
	return nil
}

// redacted is raw as an error may show it: a URL's password is masked.
func redacted(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		return u.Redacted()
	}
	return raw
}

// splitList splits a comma-separated value, trimming each item and dropping
// empty ones.
func splitList(raw string) []string {
	var out []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
