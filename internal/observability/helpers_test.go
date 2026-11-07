package observability

import (
	"sort"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// sample is one gathered series: its labels in name order and its value
// (counter, gauge or a histogram's sample count).
type sample struct {
	labels map[string]string
	value  float64
	sum    float64
}

func gather(t *testing.T, e *Exporter, name string) []sample {
	t.Helper()
	families, err := e.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []sample
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			s := sample{labels: map[string]string{}}
			for _, pair := range metric.GetLabel() {
				s.labels[pair.GetName()] = pair.GetValue()
			}
			switch family.GetType() {
			case dto.MetricType_COUNTER:
				s.value = metric.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				s.value = metric.GetGauge().GetValue()
			case dto.MetricType_HISTOGRAM:
				s.value = float64(metric.GetHistogram().GetSampleCount())
				s.sum = metric.GetHistogram().GetSampleSum()
			}
			out = append(out, s)
		}
	}
	return out
}

// value returns the series of name whose labels include want, failing when
// there is not exactly one.
func value(t *testing.T, e *Exporter, name string, want map[string]string) sample {
	t.Helper()
	var found []sample
	for _, s := range gather(t, e, name) {
		if labelsInclude(s.labels, want) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s%v: found %d series, want 1 (all: %v)", name, want, len(found), describe(gather(t, e, name)))
	}
	return found[0]
}

func absent(t *testing.T, e *Exporter, name string, want map[string]string) {
	t.Helper()
	for _, s := range gather(t, e, name) {
		if labelsInclude(s.labels, want) {
			t.Fatalf("%s%v: unexpected series %v", name, want, s.labels)
		}
	}
}

func labelsInclude(labels, want map[string]string) bool {
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func describe(samples []sample) string {
	var parts []string
	for _, s := range samples {
		keys := make([]string, 0, len(s.labels))
		for k := range s.labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var kv []string
		for _, k := range keys {
			kv = append(kv, k+"="+s.labels[k])
		}
		parts = append(parts, "{"+strings.Join(kv, ",")+"}")
	}
	return strings.Join(parts, " ")
}
