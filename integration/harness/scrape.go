package harness

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
)

// Sample is one line of a Prometheus text exposition.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Samples is one scrape.
type Samples []Sample

// Scrape reads the collector's own metrics from addr.
func Scrape(ctx context.Context, addr string) (Samples, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building scrape: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scraping %s: %w", addr, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading scrape: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape answered %d", resp.StatusCode)
	}
	return parseExposition(body)
}

// Sum adds the values of every sample called name whose labels include all
// of match.
func (s Samples) Sum(name string, match map[string]string) float64 {
	var total float64
	for _, smp := range s {
		if smp.Name != name {
			continue
		}
		if matches(smp.Labels, match) {
			total += smp.Value
		}
	}
	return total
}

// Find returns the first sample called name.
func (s Samples) Find(name string) (Sample, bool) {
	for _, smp := range s {
		if smp.Name == name {
			return smp, true
		}
	}
	return Sample{}, false
}

func matches(labels, match map[string]string) bool {
	for k, v := range match {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// parseExposition reads the subset of the text format the collector writes:
// comment lines, and `name{k="v",...} value` sample lines.
func parseExposition(body []byte) (Samples, error) {
	var out Samples
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		smp, err := parseSample(line)
		if err != nil {
			return nil, err
		}
		out = append(out, smp)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading exposition: %w", err)
	}
	return out, nil
}

func parseSample(line string) (Sample, error) {
	smp := Sample{Labels: map[string]string{}}
	var rest string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return Sample{}, fmt.Errorf("malformed sample %q", line)
		}
		smp.Name = line[:i]
		labels, err := parseLabels(line[i+1 : j])
		if err != nil {
			return Sample{}, fmt.Errorf("sample %q: %w", line, err)
		}
		maps.Copy(smp.Labels, labels)
		rest = strings.TrimSpace(line[j+1:])
	} else {
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			return Sample{}, fmt.Errorf("malformed sample %q", line)
		}
		smp.Name, rest = name, value
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Sample{}, fmt.Errorf("sample %q has no value", line)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return Sample{}, fmt.Errorf("sample %q: %w", line, err)
	}
	smp.Value = v
	return smp, nil
}

func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for s != "" {
		k, rest, ok := strings.Cut(s, "=")
		if !ok || !strings.HasPrefix(rest, `"`) {
			return nil, fmt.Errorf("malformed labels %q", s)
		}
		// Find the closing quote, skipping escaped ones.
		end := 1
		for end < len(rest) && (rest[end] != '"' || rest[end-1] == '\\') {
			end++
		}
		if end >= len(rest) {
			return nil, fmt.Errorf("unterminated label value in %q", s)
		}
		v, err := strconv.Unquote(rest[:end+1])
		if err != nil {
			return nil, fmt.Errorf("label %s: %w", k, err)
		}
		out[strings.TrimSpace(k)] = v
		s = strings.TrimPrefix(rest[end+1:], ",")
	}
	return out, nil
}
