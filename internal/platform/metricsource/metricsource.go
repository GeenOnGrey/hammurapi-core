// Package metricsource runs success-metric queries against read-only sources
// (HMR.CMN-0002 arch §14): ClickHouse (HTTP interface) and Prometheus or
// VictoriaMetrics. Queries are written by people and by the agent, so they
// are validated before execution and run with limits.
package metricsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Source is a metric source.
type Source interface {
	// Validate rejects queries that are not allowed (writes, multiple statements).
	Validate(query string) error
	// Run evaluates the query at a time and returns a single value.
	Run(ctx context.Context, query string, at time.Time) (float64, error)
	// RunRange evaluates the query over a range (next release: evaluation window).
	RunRange(ctx context.Context, query string, from, to time.Time, step time.Duration) ([]Sample, error)
}

// Sample is a value at a time.
type Sample struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// Limits bound query execution.
type Limits struct {
	MaxExecutionSeconds int           `json:"maxExecutionSeconds"`
	MaxResultRows       int           `json:"maxResultRows"`
	MaxRange            time.Duration `json:"-"`
	MaxRangeDays        int           `json:"maxRangeDays"`
}

func (l *Limits) defaults() {
	if l.MaxExecutionSeconds <= 0 {
		l.MaxExecutionSeconds = 30
	}
	if l.MaxResultRows <= 0 {
		l.MaxResultRows = 10000
	}
	if l.MaxRangeDays <= 0 {
		l.MaxRangeDays = 90
	}
	l.MaxRange = time.Duration(l.MaxRangeDays) * 24 * time.Hour
}

// Config describes a source.
type Config struct {
	Type     string // clickhouse | prometheus
	Endpoint string
	Username string
	Password string
	Limits   Limits
}

// New builds a source.
func New(c Config) (Source, error) {
	c.Limits.defaults()
	client := &http.Client{Timeout: time.Duration(c.Limits.MaxExecutionSeconds+10) * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}
	switch c.Type {
	case "clickhouse":
		return &ClickHouse{cfg: c, http: client}, nil
	case "prometheus":
		return &Prometheus{cfg: c, http: client}, nil
	}
	return nil, fmt.Errorf("unknown metric source type %q", c.Type)
}

// ─── ClickHouse ──────────────────────────────────────────────────────

// ClickHouse queries the ClickHouse HTTP interface with readonly=1.
type ClickHouse struct {
	cfg  Config
	http *http.Client
}

var (
	sqlComment = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)
	sqlString  = regexp.MustCompile(`'(?:[^'\\]|\\.)*'`)
	sqlDenied  = regexp.MustCompile(`(?i)\b(insert|alter|drop|truncate|create|attach|detach|rename|optimize|system|kill|grant|revoke|set|delete|update|exchange|outfile|backup|restore)\b`)
)

// Validate accepts a single SELECT (or WITH … SELECT) statement.
func (c *ClickHouse) Validate(query string) error {
	q := sqlComment.ReplaceAllString(query, " ")
	q = sqlString.ReplaceAllString(q, "''")
	q = strings.TrimSpace(q)
	q = strings.TrimSuffix(q, ";")
	if q == "" {
		return errors.New("empty query")
	}
	if strings.Contains(q, ";") {
		return errors.New("only one statement is allowed")
	}
	low := strings.ToLower(q)
	if !strings.HasPrefix(low, "select") && !strings.HasPrefix(low, "with") {
		return errors.New("only SELECT queries are allowed")
	}
	if m := sqlDenied.FindString(q); m != "" {
		return fmt.Errorf("%s is not allowed in a metric query", strings.ToUpper(m))
	}
	return nil
}

// Run executes the query; {at} in the query is replaced with the evaluation time.
func (c *ClickHouse) Run(ctx context.Context, query string, at time.Time) (float64, error) {
	if err := c.Validate(query); err != nil {
		return 0, err
	}
	rows, err := c.query(ctx, strings.ReplaceAll(query, "{at}", "'"+at.UTC().Format("2006-01-02 15:04:05")+"'"))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, errors.New("the query returned no rows")
	}
	return firstNumber(rows[0])
}

// RunRange evaluates the query at each step.
func (c *ClickHouse) RunRange(ctx context.Context, query string, from, to time.Time, step time.Duration) ([]Sample, error) {
	if to.Sub(from) > c.cfg.Limits.MaxRange {
		return nil, fmt.Errorf("range is longer than %d days", c.cfg.Limits.MaxRangeDays)
	}
	var out []Sample
	for t := from; !t.After(to); t = t.Add(step) {
		v, err := c.Run(ctx, query, t)
		if err != nil {
			return nil, err
		}
		out = append(out, Sample{At: t, Value: v})
	}
	return out, nil
}

func (c *ClickHouse) query(ctx context.Context, q string) ([]map[string]any, error) {
	params := url.Values{
		"readonly":             {"1"},
		"max_execution_time":   {strconv.Itoa(c.cfg.Limits.MaxExecutionSeconds)},
		"max_result_rows":      {strconv.Itoa(c.cfg.Limits.MaxResultRows)},
		"result_overflow_mode": {"throw"},
		"default_format":       {"JSON"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.Endpoint, "/")+"/?"+params.Encode(), strings.NewReader(q))
	if err != nil {
		return nil, err
	}
	if c.cfg.Username != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("clickhouse: %s", strings.TrimSpace(firstLine(string(raw))))
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return out.Data, nil
}

func firstNumber(row map[string]any) (float64, error) {
	for _, v := range row {
		switch x := v.(type) {
		case float64:
			return x, nil
		case string:
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return f, nil
			}
		}
	}
	return 0, errors.New("the first row has no numeric column")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// ─── Prometheus / VictoriaMetrics ────────────────────────────────────

// Prometheus queries the Prometheus HTTP API (query, query_range only).
type Prometheus struct {
	cfg  Config
	http *http.Client
}

// Validate accepts a non-empty PromQL expression.
func (p *Prometheus) Validate(query string) error {
	if strings.TrimSpace(query) == "" {
		return errors.New("empty query")
	}
	if len(query) > 4000 {
		return errors.New("query is too long")
	}
	return nil
}

// Run evaluates an instant query.
func (p *Prometheus) Run(ctx context.Context, query string, at time.Time) (float64, error) {
	if err := p.Validate(query); err != nil {
		return 0, err
	}
	q := url.Values{"query": {query}, "time": {strconv.FormatInt(at.Unix(), 10)}, "timeout": {fmt.Sprintf("%ds", p.cfg.Limits.MaxExecutionSeconds)}}
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/api/v1/query?"+q.Encode(), &res); err != nil {
		return 0, err
	}
	if res.Status != "success" {
		return 0, fmt.Errorf("prometheus: %s", res.Error)
	}
	if len(res.Data.Result) == 0 {
		return 0, errors.New("the query returned no series")
	}
	var point struct {
		Value []any `json:"value"`
	}
	if res.Data.ResultType == "scalar" {
		var pair []any
		_ = json.Unmarshal(res.Data.Result[0], &pair)
		point.Value = pair
	} else {
		_ = json.Unmarshal(res.Data.Result[0], &point)
	}
	if len(point.Value) != 2 {
		return 0, errors.New("unexpected result")
	}
	s, _ := point.Value[1].(string)
	return strconv.ParseFloat(s, 64)
}

// RunRange evaluates a range query within the configured maximum range (MET-03).
func (p *Prometheus) RunRange(ctx context.Context, query string, from, to time.Time, step time.Duration) ([]Sample, error) {
	if err := p.Validate(query); err != nil {
		return nil, err
	}
	if to.Sub(from) > p.cfg.Limits.MaxRange {
		return nil, fmt.Errorf("range is longer than %d days", p.cfg.Limits.MaxRangeDays)
	}
	q := url.Values{"query": {query}, "start": {strconv.FormatInt(from.Unix(), 10)}, "end": {strconv.FormatInt(to.Unix(), 10)},
		"step": {strconv.Itoa(int(step.Seconds()))}, "timeout": {fmt.Sprintf("%ds", p.cfg.Limits.MaxExecutionSeconds)}}
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Values [][]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/api/v1/query_range?"+q.Encode(), &res); err != nil {
		return nil, err
	}
	if res.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", res.Error)
	}
	var out []Sample
	if len(res.Data.Result) > 0 {
		for _, v := range res.Data.Result[0].Values {
			if len(v) != 2 {
				continue
			}
			ts, _ := v[0].(float64)
			s, _ := v[1].(string)
			f, _ := strconv.ParseFloat(s, 64)
			out = append(out, Sample{At: time.Unix(int64(ts), 0), Value: f})
		}
	}
	return out, nil
}

func (p *Prometheus) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.cfg.Endpoint, "/")+path, nil)
	if err != nil {
		return err
	}
	if p.cfg.Username != "" {
		req.SetBasicAuth(p.cfg.Username, p.cfg.Password)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("prometheus: %s", strings.TrimSpace(firstLine(string(raw))))
	}
	return nil
}
