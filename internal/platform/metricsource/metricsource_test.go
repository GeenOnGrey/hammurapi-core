package metricsource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// MET-01: writes are rejected before execution.
func TestClickHouseValidate(t *testing.T) {
	s, _ := New(Config{Type: "clickhouse", Endpoint: "http://unused"})
	ok := []string{
		"SELECT count() FROM bookings WHERE ts > {at}",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"select 'insert into' as label, 1",
		"SELECT 1; -- trailing",
	}
	for _, q := range ok {
		if err := s.Validate(q); err != nil {
			t.Errorf("%q rejected: %v", q, err)
		}
	}
	bad := []string{
		"INSERT INTO t VALUES (1)",
		"ALTER TABLE t DELETE WHERE 1",
		"SELECT 1; DROP TABLE t",
		"SELECT * FROM t INTO OUTFILE 'x'",
		"",
	}
	for _, q := range bad {
		if err := s.Validate(q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
}

func TestClickHouseRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("readonly") != "1" || r.URL.Query().Get("max_execution_time") == "" {
			http.Error(w, "limits missing", 400)
			return
		}
		w.Write([]byte(`{"data":[{"value":"0.42"}]}`))
	}))
	defer srv.Close()
	s, _ := New(Config{Type: "clickhouse", Endpoint: srv.URL})
	v, err := s.Run(context.Background(), "SELECT 0.42 AS value", time.Now())
	if err != nil || v != 0.42 {
		t.Fatalf("got %v %v", v, err)
	}
}

// MET-03: range queries longer than the limit are rejected.
func TestPrometheusRangeLimit(t *testing.T) {
	s, _ := New(Config{Type: "prometheus", Endpoint: "http://unused", Limits: Limits{MaxRangeDays: 7}})
	now := time.Now()
	if _, err := s.RunRange(context.Background(), "up", now.Add(-8*24*time.Hour), now, time.Hour); err == nil {
		t.Fatal("long range accepted")
	}
}

func TestPrometheusRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"12.5"]}]}}`))
	}))
	defer srv.Close()
	s, _ := New(Config{Type: "prometheus", Endpoint: srv.URL})
	v, err := s.Run(context.Background(), "sum(rate(x[5m]))", time.Now())
	if err != nil || v != 12.5 {
		t.Fatalf("got %v %v", v, err)
	}
}
