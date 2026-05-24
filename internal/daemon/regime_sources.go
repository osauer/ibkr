package daemon

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type regimeSeriesPoint struct {
	Date  time.Time
	Value float64
}

var regimeHTTPClient = &http.Client{Timeout: 10 * time.Second}

func fetchFREDSeries(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
	u := "https://fred.stlouisfed.org/graph/fredgraph.csv?id=" + url.QueryEscape(seriesID)
	return fetchCSVSeries(ctx, u, seriesID, "2006-01-02")
}

func fetchCBOEVVIXSeries(ctx context.Context) ([]regimeSeriesPoint, error) {
	const u = "https://cdn.cboe.com/api/global/us_indices/daily_prices/VVIX_History.csv"
	return fetchCSVSeries(ctx, u, "VVIX", "01/02/2006")
}

func fetchCSVSeries(ctx context.Context, endpoint, valueColumn, dateLayout string) ([]regimeSeriesPoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// FRED's Akamai edge has been observed resetting streams for a custom
	// product UA while accepting Go/curl defaults. Use Go's conventional UA
	// so official daily rows do not flap unavailable solely from edge policy.
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	resp, err := regimeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}

	reader := csv.NewReader(resp.Body)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read CSV header: %w", err)
	}
	valueIdx := -1
	dateIdx := -1
	for i, h := range header {
		h = strings.TrimPrefix(strings.TrimSpace(h), "\ufeff")
		switch {
		case strings.EqualFold(h, "observation_date") || strings.EqualFold(h, "DATE"):
			dateIdx = i
		case strings.EqualFold(h, valueColumn):
			valueIdx = i
		}
	}
	if dateIdx < 0 || valueIdx < 0 {
		return nil, fmt.Errorf("CSV missing date or %s column", valueColumn)
	}

	var out []regimeSeriesPoint
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read CSV row: %w", err)
		}
		if len(rec) <= dateIdx || len(rec) <= valueIdx {
			continue
		}
		rawValue := strings.TrimSpace(rec[valueIdx])
		if rawValue == "" || rawValue == "." {
			continue
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			continue
		}
		date, err := time.Parse(dateLayout, strings.TrimSpace(rec[dateIdx]))
		if err != nil {
			continue
		}
		out = append(out, regimeSeriesPoint{Date: date, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	if len(out) == 0 {
		return nil, fmt.Errorf("%s CSV contained no usable observations", valueColumn)
	}
	return out, nil
}

func latestSeriesPoint(points []regimeSeriesPoint) (regimeSeriesPoint, bool) {
	if len(points) == 0 {
		return regimeSeriesPoint{}, false
	}
	return points[len(points)-1], true
}

func laggedSeriesPoint(points []regimeSeriesPoint, observationsBack int) (regimeSeriesPoint, bool) {
	if observationsBack < 0 || len(points) <= observationsBack {
		return regimeSeriesPoint{}, false
	}
	return points[len(points)-1-observationsBack], true
}

func seriesObservationAge(date time.Time, now time.Time) time.Duration {
	if date.IsZero() {
		return 0
	}
	return now.Sub(date)
}
