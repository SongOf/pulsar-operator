package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Dashboard is everything the console's monitoring view draws.
type Dashboard struct {
	Source       string  `json:"source"`
	RangeMinutes int     `json:"rangeMinutes"`
	StepSeconds  int     `json:"stepSeconds"`
	UpdatedAt    string  `json:"updatedAt"`
	// Series maps a metric to its entities ("cluster", or one bookie or ZooKeeper pod) and their points.
	Series map[string]map[string][]Point `json:"series"`
	// Current holds the latest cluster-wide values for the stat tiles.
	Current map[string]float64 `json:"current"`
	Pods    []PodRow           `json:"pods"`
	// Errors lists queries that failed, so a partial dashboard says what is missing.
	Errors []string `json:"errors,omitempty"`
}

// PodRow is the latest state of one scraped pod.
type PodRow struct {
	Pod       string  `json:"pod"`
	Component string  `json:"component"`
	Up        bool    `json:"up"`
	HeapUsed  float64 `json:"heapUsed"`
	HeapMax   float64 `json:"heapMax"`
	CPU       float64 `json:"cpu"`
}

// panel is one chart: a PromQL template (%[1]s is the selector, %[2]s the rate window) and whether its
// result is one cluster-wide line or one line per pod.
type panel struct {
	metric, query string
	perPod        bool
}

var panels = []panel{
	{"rateIn", `sum(pulsar_broker_rate_in{%[1]s})`, false},
	{"rateOut", `sum(pulsar_broker_rate_out{%[1]s})`, false},
	{"throughputIn", `sum(pulsar_broker_throughput_in{%[1]s})`, false},
	{"throughputOut", `sum(pulsar_broker_throughput_out{%[1]s})`, false},
	{"backlog", `sum(pulsar_broker_msg_backlog{%[1]s})`, false},
	{"storageSize", `sum(pulsar_broker_storage_size{%[1]s})`, false},
	{"proxyConnections", `sum(pulsar_proxy_active_connections{%[1]s})`, false},
	{"diskUsage", `max by (pod) ({__name__=~"bookie_ledger_dir_.+_usage",%[1]s})`, true},
	{"writeBytes", `sum by (pod) (rate(bookie_WRITE_BYTES{%[1]s}[%[2]s]))`, true},
	{"readBytes", `sum by (pod) (rate(bookie_READ_BYTES{%[1]s}[%[2]s]))`, true},
	{"addP99", `max by (pod) (bookkeeper_server_ADD_ENTRY{%[1]s,success="true",quantile="0.99"})`, true},
	{"zkLatency", `max by (pod) (avg_latency{%[1]s,component="zookeeper"})`, true},
	{"zkConnections", `max by (pod) (num_alive_connections{%[1]s,component="zookeeper"})`, true},
}

// stats are instant values for the tiles.
var stats = map[string]string{
	"rateIn":          `sum(pulsar_broker_rate_in{%[1]s})`,
	"rateOut":         `sum(pulsar_broker_rate_out{%[1]s})`,
	"throughputIn":    `sum(pulsar_broker_throughput_in{%[1]s})`,
	"throughputOut":   `sum(pulsar_broker_throughput_out{%[1]s})`,
	"backlog":         `sum(pulsar_broker_msg_backlog{%[1]s})`,
	"storageSize":     `sum(pulsar_broker_storage_size{%[1]s})`,
	"topics":          `sum(pulsar_broker_topics_count{%[1]s})`,
	"producers":       `sum(pulsar_broker_producers_count{%[1]s})`,
	"consumers":       `sum(pulsar_broker_consumers_count{%[1]s})`,
	"subscriptions":   `sum(pulsar_broker_subscriptions_count{%[1]s})`,
	"bookies":         `count(up{%[1]s,component="bookie"})`,
	"writableBookies": `count(bookie_SERVER_STATUS{%[1]s} == 1)`,
	"maxDiskUsage":    `max({__name__=~"bookie_ledger_dir_.+_usage",%[1]s})`,
	"targetsDown":     `count(up{%[1]s} == 0) or vector(0)`,
}

// podQueries fill the pod table.
var podQueries = map[string]string{
	"up":       `max by (pod, component) (up{%[1]s})`,
	"heapUsed": `max by (pod) (jvm_memory_bytes_used{%[1]s,area="heap"})`,
	"heapMax":  `max by (pod) (jvm_memory_bytes_max{%[1]s,area="heap"})`,
	"cpu":      `sum by (pod) (rate(process_cpu_seconds_total{%[1]s}[%[2]s]))`,
}

// Selector picks one cluster's series by the labels the operator's scrape configuration sets.
func Selector(namespace, cluster string) string {
	return fmt.Sprintf(`namespace=%q,pulsar_cluster=%q`, namespace, cluster)
}

// StepFor keeps a chart near 240 points, never finer than the 15s scrape interval.
func StepFor(rng time.Duration) time.Duration {
	step := rng / 240
	if step < 15*time.Second {
		step = 15 * time.Second
	}
	return step.Truncate(time.Second)
}

// Build runs every query of the dashboard concurrently. A failing query leaves its panel empty and is listed
// in Errors; the dashboard fails only when Prometheus cannot be reached at all.
func Build(ctx context.Context, p Prometheus, namespace, cluster string, rng time.Duration, now time.Time) (Dashboard, error) {
	sel := Selector(namespace, cluster)
	step := StepFor(rng)
	window := "1m"
	if step > 15*time.Second {
		window = fmt.Sprintf("%ds", int(4*step.Seconds()))
	}
	q := func(tmpl string) string { return fmt.Sprintf(tmpl, sel, window) }
	d := Dashboard{Source: p.URL, RangeMinutes: int(rng.Minutes()), StepSeconds: int(step.Seconds()), UpdatedAt: now.UTC().Format(time.RFC3339),
		Series: map[string]map[string][]Point{}, Current: map[string]float64{}, Pods: []PodRow{}}

	// Reachability first, so an unreachable Prometheus is one clear error instead of twenty.
	if _, err := p.Instant(ctx, "vector(1)", now); err != nil {
		return d, err
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	fail := func(name string, err error) {
		mu.Lock()
		d.Errors = append(d.Errors, name+": "+err.Error())
		mu.Unlock()
	}
	for _, pn := range panels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Range(ctx, q(pn.query), now.Add(-rng), now, step)
			if err != nil {
				fail(pn.metric, err)
				return
			}
			entities := map[string][]Point{}
			for _, s := range res {
				if len(s.Points) == 0 {
					continue // every sample was NaN, such as a latency quantile without traffic
				}
				key := "cluster"
				if pn.perPod {
					key = s.Labels["pod"]
				}
				entities[key] = s.Points
			}
			mu.Lock()
			d.Series[pn.metric] = entities
			mu.Unlock()
		}()
	}
	for name, tmpl := range stats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Instant(ctx, q(tmpl), now)
			if err != nil {
				fail(name, err)
				return
			}
			if len(res) > 0 && len(res[0].Points) > 0 {
				mu.Lock()
				d.Current[name] = res[0].Points[0][1]
				mu.Unlock()
			}
		}()
	}
	pods := map[string]*PodRow{}
	for name, tmpl := range podQueries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Instant(ctx, q(tmpl), now)
			if err != nil {
				fail("pods "+name, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, s := range res {
				pod := s.Labels["pod"]
				if pod == "" || len(s.Points) == 0 {
					continue
				}
				row := pods[pod]
				if row == nil {
					row = &PodRow{Pod: pod}
					pods[pod] = row
				}
				v := s.Points[0][1]
				switch name {
				case "up":
					row.Up, row.Component = v == 1, s.Labels["component"]
				case "heapUsed":
					row.HeapUsed = v
				case "heapMax":
					row.HeapMax = v
				case "cpu":
					row.CPU = v
				}
			}
		}()
	}
	wg.Wait()
	for _, row := range pods {
		if row.Component != "" {
			d.Pods = append(d.Pods, *row)
		}
	}
	sort.Slice(d.Pods, func(i, j int) bool { return d.Pods[i].Pod < d.Pods[j].Pod })
	sort.Strings(d.Errors)
	return d, nil
}

// ParseRange accepts the console's ranges: 15m, 1h, 6h, 24h, 7d.
func ParseRange(s string) (time.Duration, error) {
	if s == "" {
		return time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil && days >= 1 && days <= 30 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	} else if d, err := time.ParseDuration(s); err == nil && d >= 5*time.Minute && d <= 30*24*time.Hour {
		return d, nil
	}
	return 0, fmt.Errorf("range must be between 5m and 30d, such as 1h or 7d")
}
