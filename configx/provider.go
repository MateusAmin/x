package configx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/ory/jsonschema/v3"
	"github.com/ory/x/logrusx"
	"github.com/ory/x/watcherx"

	"github.com/knadh/koanf/providers/posflag"
	"github.com/spf13/pflag"

	"github.com/ory/x/stringsx"
	"github.com/ory/x/tracing"

	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/pkg/errors"
	"github.com/rs/cors"
)

type Provider struct {
	*koanf.Koanf
	immutables        []string
	l                 *logrusx.Logger
	ctx               context.Context
	schema            []byte
	flags             *pflag.FlagSet
	validator         *jsonschema.Schema
	onChanges         func(watcherx.Event, error)
	onValidationError func(k *koanf.Koanf, err error)
}

// New creates a new provider instance or errors.
// Configuration values are loaded in the following order:
//
// 1. Defaults from the JSON Schema
// 2. Config files (yaml, yml, toml, json)
// 3. Command line flags
// 4. Environment variables
func New(schema []byte, flags *pflag.FlagSet, l *logrusx.Logger, modifiers ...OptionModifier) (*Provider, error) {
	schemaID, comp, err := newCompiler(schema)
	if err != nil {
		return nil, err
	}

	validator, err := comp.Compile(schemaID)
	if err != nil {
		return nil, err
	}

	p := &Provider{
		l:                 l,
		ctx:               context.Background(),
		schema:            schema,
		flags:             flags,
		validator:         validator,
		onChanges:         func(_ watcherx.Event, _ error) {},
		onValidationError: func(k *koanf.Koanf, err error) {},
	}

	for _, m := range modifiers {
		m(p)
	}

	k, err := p.newKoanf(p.ctx)
	if err != nil {
		return nil, err
	}
	p.Koanf = k

	return p, nil
}

func (p *Provider) validate(k *koanf.Koanf) error {
	out, err := k.Marshal(json.Parser())
	if err != nil {
		return errors.WithStack(err)
	}
	if err := p.validator.Validate(bytes.NewReader(out)); err != nil {
		p.onValidationError(k, err)
		return err
	}
	return nil
}

func (p *Provider) newKoanf(ctx context.Context) (*koanf.Koanf, error) {
	k := koanf.New(".")

	dp, err := NewKoanfSchemaDefaults(p.schema)
	if err != nil {
		return nil, err
	}

	ep, err := NewKoanfEnv("", p.schema)
	if err != nil {
		return nil, err
	}

	// Load defaults
	if err := k.Load(dp, nil); err != nil {
		return nil, err
	}

	paths, err := p.flags.GetStringSlice("config")
	for _, configFile := range paths {
		if err := p.addConfigFile(ctx, configFile, k); err != nil {
			return nil, err
		}
	}

	if err := k.Load(posflag.Provider(p.flags, ".", k), nil); err != nil {
		return nil, err
	}

	if err := k.Load(ep, nil); err != nil {
		return nil, err
	}

	if err := p.validate(k); err != nil {
		return nil, err
	}

	return k, nil
}

func (p *Provider) addConfigFile(ctx context.Context, path string, k *koanf.Koanf) error {
	var parser koanf.Parser

	switch e := filepath.Ext(path); e {
	case ".toml":
		parser = toml.Parser()
	case ".json":
		parser = json.Parser()
	case ".yaml", ".yml":
		parser = yaml.Parser()
	default:
		return errors.Errorf("unknown config file extension: %s", e)
	}

	ctx, cancel := context.WithCancel(p.ctx)
	fp := NewKoanfFile(ctx, path)

	c := make(watcherx.EventChannel)
	go func(c watcherx.EventChannel) {
		for e := range c {
			p.l.WithField("file", e.Source()).
				WithField("event", e).
				WithField("event_type", fmt.Sprintf("%T", e)).
				WithField("immutables", p.immutables).
				Info("A change to a configuration file was detected.")

			switch et := e.(type) {
			case *watcherx.ErrorEvent:
				p.l.WithError(et).Errorf("An error occurred while watching config file %s", path)
			default: // *watcherx.RemoveEvent, *watcherx.ChangeEvent
				ctx, cancelInner := context.WithCancel(ctx)

				var cancelReload bool
				nk, err := p.newKoanf(ctx)
				if err != nil {
					cancelReload = true
					p.l.WithError(err).WithField("event", fmt.Sprintf("%#v", et)).
						Errorf("The changed configuration is invalid and could not be loaded. Rolling back to the last working configuration revision. Please address the validation errors before restarting the process.")
				} else {
					for _, key := range p.immutables {
						if !reflect.DeepEqual(k.Get(key), nk.Get(key)) {
							err = errors.Errorf("immutable configuration key \"%s\" was changed", key)
							cancelReload = true
							p.l.WithError(err).
								WithField("key", key).
								WithField("old_value", fmt.Sprintf("%v", k.Get(key))).
								WithField("new_value", fmt.Sprintf("%v", nk.Get(key))).
								Errorf("A configuration value marked as immutable has changed. Rolling back to the last working configuration revision. To reload the values please restart the process.")
							break
						}
					}
				}

				if cancelReload {
					cancelInner()
					p.onChanges(e, err)
					continue
				}

				p.Koanf = nk
				cancel()
				cancel = cancelInner
				p.onChanges(e, nil)
				close(c)
				return
			}
		}
	}(c)

	if err := fp.WatchChannel(c); err != nil {
		close(c)
		return err
	}

	return k.Load(fp, parser)
}

func (p *Provider) Set(key string, value interface{}) {
	// This can not err because confmap does not err
	_ = p.Koanf.Load(confmap.Provider(map[string]interface{}{
		key: value}, "."), nil)
}

func (p *Provider) BoolF(key string, fallback bool) bool {
	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Bool(key)
}

func (p *Provider) StringF(key string, fallback string) string {
	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.String(key)
}

func (p *Provider) StringsF(key string, fallback []string) (val []string) {
	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Strings(key)
}

func (p *Provider) IntF(key string, fallback int) (val int) {
	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Int(key)
}

func (p *Provider) Float64F(key string, fallback float64) (val float64) {
	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Float64(key)
}

func (p *Provider) DurationF(key string, fallback time.Duration) (val time.Duration) {
	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Duration(key)
}

func (p *Provider) GetF(key string, fallback interface{}) (val interface{}) {
	val = p.Get(key)
	if val == nil {
		return fallback
	}

	return val
}

func (p *Provider) CORS(prefix string, defaults cors.Options) (cors.Options, bool) {
	if len(prefix) > 0 {
		prefix = strings.TrimRight(prefix, ".") + "."
	}

	return cors.Options{
		AllowedOrigins:     p.StringsF(prefix+"cors.allowed_origins", defaults.AllowedOrigins),
		AllowedMethods:     p.StringsF(prefix+"cors.allowed_methods", defaults.AllowedMethods),
		AllowedHeaders:     p.StringsF(prefix+"cors.allowed_headers", defaults.AllowedHeaders),
		ExposedHeaders:     p.StringsF(prefix+"cors.exposed_headers", defaults.ExposedHeaders),
		AllowCredentials:   p.BoolF(prefix+"cors.allow_credentials", defaults.AllowCredentials),
		OptionsPassthrough: p.BoolF(prefix+"cors.options_passthrough", defaults.OptionsPassthrough),
		MaxAge:             p.IntF(prefix+"cors.max_age", defaults.MaxAge),
		Debug:              p.BoolF(prefix+"cors.debug", defaults.Debug),
	}, p.Bool(prefix + "cors.enabled")
}

func (p *Provider) TracingConfig(serviceName string) *tracing.Config {
	return &tracing.Config{
		ServiceName: p.StringF("tracing.service_name", serviceName),
		Provider:    p.String("tracing.provider"),
		Jaeger: &tracing.JaegerConfig{
			LocalAgentHostPort: p.String("tracing.providers.jaeger.local_agent_address"),
			SamplerType:        p.StringF("tracing.providers.jaeger.sampling.type", "const"),
			SamplerValue:       p.Float64F("tracing.providers.jaeger.sampling.value", float64(1)),
			SamplerServerURL:   p.String("tracing.providers.jaeger.sampling.server_url"),
			Propagation: stringsx.Coalesce(
				os.Getenv("JAEGER_PROPAGATION"),
				p.String("tracing.providers.jaeger.propagation"),
			),
		},
		Zipkin: &tracing.ZipkinConfig{
			ServerURL: p.String("tracing.providers.zipkin.server_url"),
		},
	}
}

func (p *Provider) RequestURIF(path string, fallback *url.URL) *url.URL {
	if p.Get(path) == nil {
		return fallback
	}

	parsed, err := url.ParseRequestURI(p.String(path))
	if err != nil {
		return fallback
	}

	return parsed
}

func (p *Provider) URIF(path string, fallback *url.URL) *url.URL {
	if p.Get(path) == nil {
		return fallback
	}

	parsed, err := url.Parse(p.String(path))
	if err != nil {
		return fallback
	}

	return parsed
}

// PrintHumanReadableValidationErrors prints human readable validation errors. Duh.
func (p *Provider) PrintHumanReadableValidationErrors(w io.Writer, err error) {
	p.printHumanReadableValidationErrors(p.Koanf, w, err)
}

func (p *Provider) printHumanReadableValidationErrors(k *koanf.Koanf, w io.Writer, err error) {
	if err == nil {
		return
	}

	_, _ = fmt.Fprintln(os.Stderr, "")
	conf, innerErr := k.Marshal(json.Parser())
	if innerErr != nil {
		p.l.WithError(innerErr).Error("Unable to unmarshal configuration.")
	}
	p.formatValidationErrorForCLI(w, conf, err)
}