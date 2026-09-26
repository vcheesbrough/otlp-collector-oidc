package render

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// templateText is the shipped pipeline, with a placeholder for every
// variable in Settings.
//
//go:embed collector.yaml.tmpl
var templateText string

// Render is the shipped pipeline for s, as collector YAML.
func Render(s Settings) ([]byte, error) {
	tmpl, err := parseTemplate()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, s); err != nil {
		return nil, fmt.Errorf("rendering the collector configuration: %w", err)
	}
	return out.Bytes(), nil
}

func parseTemplate() (*template.Template, error) {
	tmpl, err := template.New("collector.yaml.tmpl").
		Option("missingkey=error").
		Funcs(template.FuncMap{
			"q":                  quote,
			"list":               quoteList,
			"identityActions":    identityActions,
			"identityProcessors": identityProcessors,
			"resourceActions":    resourceActions,
			"serviceNameFilter":  serviceNameFilter,
			"pastAgeSpanFilter":  pastAgeSpanFilter,
			"pastAgeLogFilter":   pastAgeLogFilter,
			"spanClamps":         futureSkewSpanClamps,
			"logClamps":          futureSkewLogClamps,
			"metricNameFilter":   metricNameFilter,
			"metricResource":     metricResourceReduce,
			"metricKeepKeys":     metricKeepKeys,
			"metricProcessors":   metricProcessors,
			"join":               func(items []string) string { return strings.Join(items, ", ") },
		}).
		Parse(templateText)
	if err != nil {
		return nil, fmt.Errorf("parsing the collector template: %w", err)
	}
	return tmpl, nil
}

// quote renders v as a YAML double-quoted scalar. JSON's string syntax is a
// subset of YAML's, so no value can end the scalar early; "$" is doubled so
// the collector's ${...} expansion reads it back as a literal "$".
func quote(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fmt.Sprint(v)); err != nil {
		return "", fmt.Errorf("quoting a value: %w", err)
	}
	return strings.ReplaceAll(strings.TrimSuffix(buf.String(), "\n"), "$", "$$"), nil
}

// quoteList renders items as a YAML flow sequence of quoted scalars.
func quoteList(items []string) (string, error) {
	quoted := make([]string, len(items))
	for i, item := range items {
		q, err := quote(item)
		if err != nil {
			return "", err
		}
		quoted[i] = q
	}
	return "[" + strings.Join(quoted, ", ") + "]", nil
}
