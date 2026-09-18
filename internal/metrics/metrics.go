// Package metrics renders samples in the Prometheus text exposition format.
// It is a snapshot writer, not a registry: the caller produces the current
// samples on every scrape, so series that disappear simply stop being written.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type Label struct {
	Name, Value string
}

type Sample struct {
	Name   string
	Labels []Label
	Value  float64
}

// Family describes one metric name; Type is "gauge" or "counter".
type Family struct {
	Name, Type, Help string
}

// Write renders every family's HELP and TYPE lines followed by its samples,
// sorted by name and labels so the output is stable.
func Write(w io.Writer, families []Family, samples []Sample) error {
	byName := map[string][]Sample{}
	for _, s := range samples {
		byName[s.Name] = append(byName[s.Name], s)
	}
	bw := bufio.NewWriter(w)
	for _, f := range families {
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s %s\n", f.Name, f.Help, f.Name, f.Type)
		group := byName[f.Name]
		slices.SortFunc(group, func(a, b Sample) int { return strings.Compare(labelSet(a.Labels), labelSet(b.Labels)) })
		for _, s := range group {
			fmt.Fprintf(bw, "%s%s %s\n", s.Name, labelSet(s.Labels), strconv.FormatFloat(s.Value, 'g', -1, 64))
		}
	}
	return bw.Flush()
}

// Handler serves a fresh snapshot on every request.
func Handler(families []Family, snapshot func() []Sample) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		Write(w, families, snapshot())
	})
}

func labelSet(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, len(labels))
	for i, l := range labels {
		parts[i] = l.Name + `="` + escape(l.Value) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(s string) string { return escaper.Replace(s) }
