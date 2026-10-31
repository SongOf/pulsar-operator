// Package metrics builds the console's monitoring dashboard from Prometheus, which keeps the samples on a
// persistent volume (see internal/resources/monitoring.go).
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Prometheus queries the HTTP API of one Prometheus.
type Prometheus struct {
	URL  string
	HTTP *http.Client
}

// Series is one result series: its labels and points.
type Series struct {
	Labels map[string]string
	Points []Point
}

// Point is a time (Unix seconds) and a value.
type Point [2]float64

type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// Range runs a range query.
func (p Prometheus) Range(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	q := url.Values{"query": {query}, "start": {unix(start)}, "end": {unix(end)}, "step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}}
	return p.do(ctx, "/api/v1/query_range", q)
}

// Instant runs an instant query.
func (p Prometheus) Instant(ctx context.Context, query string, at time.Time) ([]Series, error) {
	return p.do(ctx, "/api/v1/query", url.Values{"query": {query}, "time": {unix(at)}})
}

func unix(t time.Time) string { return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64) }

func (p Prometheus) do(ctx context.Context, path string, q url.Values) ([]Series, error) {
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("prometheus answered %s: %.200s", resp.Status, body)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus %s: %s", r.ErrorType, r.Error)
	}
	out := make([]Series, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		s := Series{Labels: res.Metric}
		raw := res.Values
		if res.Value != nil {
			raw = [][]any{res.Value}
		}
		for _, v := range raw {
			if pt, ok := point(v); ok {
				s.Points = append(s.Points, pt)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// point decodes [time, "value"], dropping NaN and infinities, which JSON cannot carry to the console.
func point(v []any) (Point, bool) {
	if len(v) != 2 {
		return Point{}, false
	}
	t, ok := v[0].(float64)
	str, ok2 := v[1].(string)
	if !ok || !ok2 {
		return Point{}, false
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return Point{}, false
	}
	return Point{t, f}, true
}
