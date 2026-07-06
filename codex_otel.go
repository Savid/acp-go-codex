package codexacp

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	codexOTELEnvEnvironment   = "CODEX_OTEL_ENVIRONMENT"
	codexOTELEnvLogUserPrompt = "CODEX_OTEL_LOG_USER_PROMPT"

	otelEnvExporterOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	otelEnvExporterOTLPProtocol = "OTEL_EXPORTER_OTLP_PROTOCOL"
	otelEnvExporterOTLPHeaders  = "OTEL_EXPORTER_OTLP_HEADERS"
	otelEnvResourceAttributes   = "OTEL_RESOURCE_ATTRIBUTES"

	otelEnvDeploymentEnvironmentName = "deployment.environment.name"
	otelEnvDeploymentEnvironment     = "deployment.environment"

	otelExporterNone     = "none"
	otelExporterOTLP     = "otlp"
	otelExporterOTLPHTTP = "otlp-http"
	otelExporterOTLPGRPC = "otlp-grpc"

	otelProtocolHTTPProtobuf = "http/protobuf"
	otelProtocolHTTPJSON     = "http/json"
	otelProtocolGRPC         = "grpc"

	otelCodexProtocolBinary = "binary"
	otelCodexProtocolJSON   = "json"

	envOTELLogsExporter    = "OTEL_LOGS_EXPORTER"
	envOTELLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	envOTELLogsProtocol    = "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"
	envOTELTracesExporter  = "OTEL_TRACES_EXPORTER"
	envOTELTracesProtocol  = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envOTELMetricsExporter = "OTEL_METRICS_EXPORTER"
	envOTELMetricsProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"

	otelValueYes   = "yes"
	otelSchemeHTTP = "http"
)

var osEnviron = os.Environ

type codexOTELConfig struct {
	Enabled   bool
	ExtraArgs []string
}

type codexOTELSignal struct {
	Name            string
	ExporterEnv     string
	EndpointEnv     string
	ProtocolEnv     string
	CertificateEnv  string
	ClientCertEnv   string
	ClientKeyEnv    string
	SelectorKey     string
	HTTPPath        string
	AllowGenericEnv bool
}

type codexOTELSignalConfig struct {
	Enabled           bool
	Exporter          string
	Endpoint          string
	HTTPProtocol      string
	CACertificate     string
	ClientCertificate string
	ClientKey         string
}

var (
	codexOTELLogsSignal = codexOTELSignal{
		Name:           "logs",
		ExporterEnv:    envOTELLogsExporter,
		EndpointEnv:    envOTELLogsEndpoint,
		ProtocolEnv:    envOTELLogsProtocol,
		CertificateEnv: "OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE",
		ClientCertEnv:  "OTEL_EXPORTER_OTLP_LOGS_CLIENT_CERTIFICATE",
		ClientKeyEnv:   "OTEL_EXPORTER_OTLP_LOGS_CLIENT_KEY",
		SelectorKey:    "otel.exporter",
		HTTPPath:       "/v1/logs",
	}
	codexOTELTracesSignal = codexOTELSignal{
		Name:            "traces",
		ExporterEnv:     envOTELTracesExporter,
		EndpointEnv:     "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		ProtocolEnv:     envOTELTracesProtocol,
		CertificateEnv:  "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		ClientCertEnv:   "OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		ClientKeyEnv:    "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
		SelectorKey:     "otel.trace_exporter",
		HTTPPath:        "/v1/traces",
		AllowGenericEnv: true,
	}
	codexOTELMetricsSignal = codexOTELSignal{
		Name:            "metrics",
		ExporterEnv:     envOTELMetricsExporter,
		EndpointEnv:     "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		ProtocolEnv:     envOTELMetricsProtocol,
		CertificateEnv:  "OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE",
		ClientCertEnv:   "OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE",
		ClientKeyEnv:    "OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY",
		HTTPPath:        "/v1/metrics",
		AllowGenericEnv: true,
	}
)

func (a *Agent) codexOTELConfig(envOverlay map[string]string) (codexOTELConfig, error) {
	env := codexOTELEffectiveEnv(a.options.Env, envOverlay)

	return codexOTELConfigFromEnv(env)
}

func codexOTELEffectiveEnv(agentEnv map[string]string, sessionEnv map[string]string) map[string]string {
	env := envMapFromEnviron(osEnviron())
	overlayStringMap(env, agentEnv)
	overlayStringMap(env, sessionEnv)

	return env
}

func codexOTELConfigFromEnv(env map[string]string) (codexOTELConfig, error) {
	logs, err := codexOTELSignalFromEnv(env, codexOTELLogsSignal)
	if err != nil {
		return codexOTELConfig{}, err
	}

	traces, err := codexOTELSignalFromEnv(env, codexOTELTracesSignal)
	if err != nil {
		return codexOTELConfig{}, err
	}

	metrics, err := codexOTELSignalFromEnv(env, codexOTELMetricsSignal)
	if err != nil {
		return codexOTELConfig{}, err
	}

	if !logs.Enabled && !traces.Enabled && !metrics.Enabled {
		return codexOTELConfig{}, nil
	}

	args := []string{"--analytics-default-enabled"}
	if environment := codexOTELEnvironment(env); environment != "" {
		args = append(args, codexConfigArg("otel.environment", environment)...)
	}

	args = append(args, codexOTELLogArgs(logs, codexOTELLogsSignal)...)

	args = append(args, codexOTELLogArgs(traces, codexOTELTracesSignal)...)
	if metrics.Enabled {
		args = append(args, codexConfigArg("otel.metrics_exporter", codexOTELMetricsExporterLiteral(metrics))...)
	}

	args = append(args, codexConfigArg("otel.log_user_prompt", codexOTELLogUserPrompt(env))...)

	return codexOTELConfig{Enabled: true, ExtraArgs: args}, nil
}

func codexOTELSignalFromEnv(env map[string]string, signal codexOTELSignal) (codexOTELSignalConfig, error) {
	if !codexOTELSignalEnabled(env, signal) {
		return codexOTELSignalConfig{}, nil
	}

	exporter, protocol, err := codexOTELExporter(env, signal)
	if err != nil {
		return codexOTELSignalConfig{}, err
	}

	config := codexOTELSignalConfig{
		Enabled:           true,
		Exporter:          exporter,
		Endpoint:          codexOTELEndpoint(env, signal, exporter),
		HTTPProtocol:      protocol,
		CACertificate:     codexOTELSignalSpecific(env, signal.CertificateEnv, "OTEL_EXPORTER_OTLP_CERTIFICATE"),
		ClientCertificate: codexOTELSignalSpecific(env, signal.ClientCertEnv, "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"),
		ClientKey:         codexOTELSignalSpecific(env, signal.ClientKeyEnv, "OTEL_EXPORTER_OTLP_CLIENT_KEY"),
	}
	if !codexOTELSupportsClientCertificate(exporter) {
		config.ClientCertificate = ""
		config.ClientKey = ""
	}

	if config.ClientCertificate == "" || config.ClientKey == "" {
		config.ClientCertificate = ""
		config.ClientKey = ""
	}

	return config, nil
}

func codexOTELSignalEnabled(env map[string]string, signal codexOTELSignal) bool {
	if exporter := strings.ToLower(strings.TrimSpace(env[signal.ExporterEnv])); exporter != "" {
		return exporter == otelExporterOTLP
	}

	if strings.TrimSpace(env[signal.EndpointEnv]) != "" {
		return true
	}

	if !signal.AllowGenericEnv {
		return false
	}

	return codexOTELGenericConfigured(env)
}

func codexOTELGenericConfigured(env map[string]string) bool {
	return strings.TrimSpace(env[otelEnvExporterOTLPEndpoint]) != "" ||
		strings.TrimSpace(env[otelEnvExporterOTLPProtocol]) != ""
}

func codexOTELExporter(env map[string]string, signal codexOTELSignal) (string, string, error) {
	protocol := strings.ToLower(strings.TrimSpace(codexOTELSignalSpecific(env, signal.ProtocolEnv, otelEnvExporterOTLPProtocol)))
	if protocol == "" {
		protocol = otelProtocolHTTPProtobuf
	}

	switch protocol {
	case otelProtocolHTTPProtobuf:
		return otelExporterOTLPHTTP, otelCodexProtocolBinary, nil
	case otelProtocolHTTPJSON:
		return otelExporterOTLPHTTP, otelCodexProtocolJSON, nil
	case otelProtocolGRPC:
		return otelExporterOTLPGRPC, "", nil
	default:
		return "", "", fmt.Errorf("%s has unsupported OTLP protocol %q", signal.ProtocolEnv, protocol)
	}
}

func codexOTELEndpoint(env map[string]string, signal codexOTELSignal, exporter string) string {
	if endpoint := strings.TrimSpace(env[signal.EndpointEnv]); endpoint != "" {
		return endpoint
	}

	if endpoint := strings.TrimSpace(env[otelEnvExporterOTLPEndpoint]); endpoint != "" {
		if exporter == otelExporterOTLPHTTP {
			return appendOTLPSignalPath(endpoint, signal.HTTPPath)
		}

		return endpoint
	}

	if exporter == otelExporterOTLPGRPC {
		return "http://localhost:4317"
	}

	return "http://localhost:4318" + signal.HTTPPath
}

func codexOTELLogArgs(config codexOTELSignalConfig, signal codexOTELSignal) []string {
	if !config.Enabled {
		return nil
	}

	args := codexConfigArg(signal.SelectorKey, config.Exporter)
	prefix := signal.SelectorKey + "." + config.Exporter

	args = append(args, codexConfigArg(prefix+".endpoint", config.Endpoint)...)
	if config.Exporter == otelExporterOTLPHTTP {
		args = append(args, codexConfigArg(prefix+".protocol", config.HTTPProtocol)...)
	}

	args = append(args, codexOTELTLSArgs(prefix, config)...)

	return args
}

func codexOTELTLSArgs(prefix string, config codexOTELSignalConfig) []string {
	args := []string{}
	if config.CACertificate != "" {
		args = append(args, codexConfigArg(prefix+".tls.ca-certificate", config.CACertificate)...)
	}

	if config.ClientCertificate != "" && config.ClientKey != "" {
		args = append(args, codexConfigArg(prefix+".tls.client-certificate", config.ClientCertificate)...)
		args = append(args, codexConfigArg(prefix+".tls.client-private-key", config.ClientKey)...)
	}

	return args
}

func codexOTELMetricsExporterLiteral(config codexOTELSignalConfig) tomlLiteral {
	fields := []string{"endpoint = " + tomlString(config.Endpoint)}
	if config.Exporter == otelExporterOTLPHTTP {
		fields = append(fields, "protocol = "+tomlString(config.HTTPProtocol))
	}

	if tls := codexOTELMetricsTLSLiteral(config); tls != "" {
		fields = append(fields, "tls = "+tls)
	}

	return tomlLiteral("{ " + tomlString(config.Exporter) + " = { " + strings.Join(fields, ", ") + " } }")
}

func codexOTELMetricsTLSLiteral(config codexOTELSignalConfig) string {
	fields := []string{}
	if config.CACertificate != "" {
		fields = append(fields, "ca-certificate = "+tomlString(config.CACertificate))
	}

	if config.ClientCertificate != "" && config.ClientKey != "" {
		fields = append(fields,
			"client-certificate = "+tomlString(config.ClientCertificate),
			"client-private-key = "+tomlString(config.ClientKey),
		)
	}

	if len(fields) == 0 {
		return ""
	}

	return "{ " + strings.Join(fields, ", ") + " }"
}

func codexOTELSupportsClientCertificate(exporter string) bool {
	return exporter == otelExporterOTLPGRPC
}

func codexOTELEnvironment(env map[string]string) string {
	if value := strings.TrimSpace(env[codexOTELEnvEnvironment]); value != "" {
		return value
	}

	attrs := parseOTELResourceAttributes(env[otelEnvResourceAttributes])
	if value := strings.TrimSpace(attrs[otelEnvDeploymentEnvironmentName]); value != "" {
		return value
	}

	return strings.TrimSpace(attrs[otelEnvDeploymentEnvironment])
}

func parseOTELResourceAttributes(value string) map[string]string {
	out := map[string]string{}

	for _, item := range strings.Split(value, ",") {
		key, rawValue, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		out[key] = strings.TrimSpace(rawValue)
	}

	return out
}

func codexOTELLogUserPrompt(env map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(env[codexOTELEnvLogUserPrompt])) {
	case "1", "true", otelValueYes:
		return true
	default:
		return false
	}
}

func codexOTELSignalSpecific(env map[string]string, signalKey string, genericKey string) string {
	if value := strings.TrimSpace(env[signalKey]); value != "" {
		return value
	}

	return strings.TrimSpace(env[genericKey])
}

func appendOTLPSignalPath(endpoint string, signalPath string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != otelSchemeHTTP && parsed.Scheme != "https") {
		trimmed := strings.TrimRight(endpoint, "/")
		if strings.HasSuffix(trimmed, signalPath) {
			return endpoint
		}

		return trimmed + signalPath
	}

	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), signalPath) {
		return endpoint
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/") + signalPath

	return parsed.String()
}

func envMapFromEnviron(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}

		env[key] = value
	}

	return env
}

func overlayStringMap(base map[string]string, overlay map[string]string) {
	for key, value := range overlay {
		base[key] = value
	}
}
