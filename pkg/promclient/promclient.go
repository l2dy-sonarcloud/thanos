// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

// Package promclient offers helper client function for various API endpoints.

package promclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/status"
	"github.com/pkg/errors"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"google.golang.org/grpc/codes"
	"gopkg.in/yaml.v2"

	"github.com/thanos-io/thanos/pkg/clientconfig"
	"github.com/thanos-io/thanos/pkg/exemplars/exemplarspb"
	"github.com/thanos-io/thanos/pkg/metadata/metadatapb"
	"github.com/thanos-io/thanos/pkg/rules/rulespb"
	"github.com/thanos-io/thanos/pkg/runutil"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/targets/targetspb"
	"github.com/thanos-io/thanos/pkg/tracing"
)

var (
	ErrFlagEndpointNotFound = errors.New("no flag endpoint found")

	statusToCode = map[int]codes.Code{
		http.StatusBadRequest:          codes.InvalidArgument,
		http.StatusNotFound:            codes.NotFound,
		http.StatusUnprocessableEntity: codes.Internal,
		http.StatusServiceUnavailable:  codes.Unavailable,
		http.StatusInternalServerError: codes.Internal,
	}
)

const (
	SUCCESS = "success"
)

// HTTPClient sends an HTTP request and returns the response.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Client represents a Prometheus API client.
type Client struct {
	HTTPClient
	userAgent string
	logger    log.Logger
}

// NewClient returns a new Prometheus API client.
func NewClient(c HTTPClient, logger log.Logger, userAgent string) *Client {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	return &Client{
		HTTPClient: c,
		logger:     logger,
		userAgent:  userAgent,
	}
}

// NewDefaultClient returns Client with tracing tripperware.
func NewDefaultClient() *Client {
	client, _ := clientconfig.NewHTTPClient(clientconfig.HTTPClientConfig{}, "")
	return NewWithTracingClient(
		log.NewNopLogger(),
		client,
		"",
	)
}

// NewWithTracingClient returns client with tracing tripperware.
func NewWithTracingClient(logger log.Logger, httpClient *http.Client, userAgent string) *Client {
	httpClient.Transport = tracing.HTTPTripperware(log.NewNopLogger(), httpClient.Transport)
	return NewClient(
		httpClient,
		logger,
		userAgent,
	)
}

// req2xx sends a request to the given url.URL. If method is http.MethodPost then
// the raw query is encoded in the body and the appropriate Content-Type is set.
func (c *Client) req2xx(ctx context.Context, u *url.URL, method string, headers http.Header) (_ []byte, _ int, err error) {
	var b io.Reader
	if method == http.MethodPost {
		rq := u.RawQuery
		b = strings.NewReader(rq)
		u.RawQuery = ""
	}

	req, err := http.NewRequest(method, u.String(), b)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "create %s request", method)
	}
	if headers != nil {
		req.Header = headers
	}

	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, errors.Wrapf(err, "perform %s request against %s", method, u.String())
	}
	defer runutil.ExhaustCloseWithErrCapture(&err, resp.Body, "%s: close body", req.URL.String())

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, errors.Wrap(err, "read body")
	}
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, errors.Errorf("expected 2xx response, got %d. Body: %v", resp.StatusCode, string(body))
	}
	return body, resp.StatusCode, nil
}

// IsWALDirAccessible returns no error if WAL dir can be found. This helps to tell
// if we have access to Prometheus TSDB directory.
func IsWALDirAccessible(dir string) error {
	const errMsg = "WAL dir is not accessible. Is this dir a TSDB directory? If yes it is shared with TSDB?"

	f, err := os.Stat(filepath.Join(dir, "wal"))
	if err != nil {
		return errors.Wrap(err, errMsg)
	}

	if !f.IsDir() {
		return errors.New(errMsg)
	}

	return nil
}

// IsDirAccessible returns no error if dir can be found.
func IsDirAccessible(dir string) error {
	const errMsg = "Dir is not accessible."

	f, err := os.Stat(dir)
	if err != nil {
		return errors.Wrap(err, errMsg)
	}

	if !f.IsDir() {
		return errors.New(errMsg)
	}

	return nil
}

// ExternalLabels returns sorted external labels from /api/v1/status/config Prometheus endpoint.
// Note that configuration can be hot reloadable on Prometheus, so this config might change in runtime.
func (c *Client) ExternalLabels(ctx context.Context, base *url.URL) (labels.Labels, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/status/config")

	span, ctx := tracing.StartSpan(ctx, "/prom_config HTTP[client]")
	defer span.Finish()

	body, _, err := c.req2xx(ctx, &u, http.MethodGet, nil)
	if err != nil {
		return labels.EmptyLabels(), err
	}
	var d struct {
		Data struct {
			YAML string `json:"yaml"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return labels.EmptyLabels(), errors.Wrapf(err, "unmarshal response: %v", string(body))
	}
	var cfg struct {
		GlobalConfig struct {
			ExternalLabels map[string]string `yaml:"external_labels"`
		} `yaml:"global"`
	}
	if err := yaml.Unmarshal([]byte(d.Data.YAML), &cfg); err != nil {
		return labels.EmptyLabels(), errors.Wrapf(err, "parse Prometheus config: %v", d.Data.YAML)
	}

	return labels.FromMap(cfg.GlobalConfig.ExternalLabels), nil
}

type Flags struct {
	TSDBPath           string         `json:"storage.tsdb.path"`
	TSDBRetention      model.Duration `json:"storage.tsdb.retention"`
	TSDBMinTime        model.Duration `json:"storage.tsdb.min-block-duration"`
	TSDBMaxTime        model.Duration `json:"storage.tsdb.max-block-duration"`
	WebEnableAdminAPI  bool           `json:"web.enable-admin-api"`
	WebEnableLifecycle bool           `json:"web.enable-lifecycle"`
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (f *Flags) UnmarshalJSON(b []byte) error {
	// TODO(bwplotka): Avoid this custom unmarshal by:
	// - prometheus/common: adding unmarshalJSON to modelDuration
	// - prometheus/prometheus: flags should return proper JSON (not bool in string).
	parsableFlags := struct {
		TSDBPath           string        `json:"storage.tsdb.path"`
		TSDBRetention      modelDuration `json:"storage.tsdb.retention"`
		TSDBMinTime        modelDuration `json:"storage.tsdb.min-block-duration"`
		TSDBMaxTime        modelDuration `json:"storage.tsdb.max-block-duration"`
		WebEnableAdminAPI  modelBool     `json:"web.enable-admin-api"`
		WebEnableLifecycle modelBool     `json:"web.enable-lifecycle"`
	}{}

	if err := json.Unmarshal(b, &parsableFlags); err != nil {
		return err
	}

	*f = Flags{
		TSDBPath:           parsableFlags.TSDBPath,
		TSDBRetention:      model.Duration(parsableFlags.TSDBRetention),
		TSDBMinTime:        model.Duration(parsableFlags.TSDBMinTime),
		TSDBMaxTime:        model.Duration(parsableFlags.TSDBMaxTime),
		WebEnableAdminAPI:  bool(parsableFlags.WebEnableAdminAPI),
		WebEnableLifecycle: bool(parsableFlags.WebEnableLifecycle),
	}
	return nil
}

type modelDuration model.Duration

// UnmarshalJSON implements the json.Unmarshaler interface.
func (d *modelDuration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	dur, err := model.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = modelDuration(dur)
	return nil
}

type modelBool bool

// UnmarshalJSON implements the json.Unmarshaler interface.
func (m *modelBool) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	boolean, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*m = modelBool(boolean)
	return nil
}

// ConfiguredFlags returns configured flags from /api/v1/status/flags Prometheus endpoint.
// Added to Prometheus from v2.2.
func (c *Client) ConfiguredFlags(ctx context.Context, base *url.URL) (Flags, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/status/flags")

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return Flags{}, errors.Wrap(err, "create request")
	}

	span, ctx := tracing.StartSpan(ctx, "/prom_flags HTTP[client]")
	defer span.Finish()

	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return Flags{}, errors.Wrapf(err, "request config against %s", u.String())
	}
	defer runutil.ExhaustCloseWithLogOnErr(c.logger, resp.Body, "query body")

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Flags{}, errors.New("failed to read body")
	}

	switch resp.StatusCode {
	case 404:
		return Flags{}, ErrFlagEndpointNotFound
	case 200:
		var d struct {
			Data Flags `json:"data"`
		}

		if err := json.Unmarshal(b, &d); err != nil {
			return Flags{}, errors.Wrapf(err, "unmarshal response: %v", string(b))
		}

		return d.Data, nil
	default:
		return Flags{}, errors.Errorf("got non-200 response code: %v, response: %v", resp.StatusCode, string(b))
	}

}

// Snapshot will request Prometheus to perform snapshot in directory returned by this function.
// Returned directory is relative to Prometheus data-dir.
// NOTE: `--web.enable-admin-api` flag has to be set on Prometheus.
// Added to Prometheus from v2.1.
// TODO(bwplotka): Add metrics.
func (c *Client) Snapshot(ctx context.Context, base *url.URL, skipHead bool) (string, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/admin/tsdb/snapshot")

	req, err := http.NewRequest(
		http.MethodPost,
		u.String(),
		strings.NewReader(url.Values{"skip_head": []string{strconv.FormatBool(skipHead)}}.Encode()),
	)
	if err != nil {
		return "", errors.Wrap(err, "create request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	span, ctx := tracing.StartSpan(ctx, "/prom_snapshot HTTP[client]")
	defer span.Finish()

	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return "", errors.Wrapf(err, "request snapshot against %s", u.String())
	}
	defer runutil.ExhaustCloseWithLogOnErr(c.logger, resp.Body, "query body")

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.New("failed to read body")
	}

	if resp.StatusCode != 200 {
		return "", errors.Errorf("is 'web.enable-admin-api' flag enabled? got non-200 response code: %v, response: %v", resp.StatusCode, string(b))
	}

	var d struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return "", errors.Wrapf(err, "unmarshal response: %v", string(b))
	}

	return path.Join("snapshots", d.Data.Name), nil
}

type QueryOptions struct {
	DoNotAddThanosParams    bool
	Deduplicate             bool
	PartialResponseStrategy storepb.PartialResponseStrategy
	Method                  string
	MaxSourceResolution     string
	Engine                  string
	Explain                 bool
	Analyze                 bool
	HTTPHeaders             http.Header
}

func (p *QueryOptions) AddTo(values url.Values) error {
	values.Add("dedup", fmt.Sprintf("%v", p.Deduplicate))
	if len(p.MaxSourceResolution) > 0 {
		values.Add("max_source_resolution", p.MaxSourceResolution)
	}

	values.Add("explain", fmt.Sprintf("%v", p.Explain))
	values.Add("analyze", fmt.Sprintf("%v", p.Analyze))
	values.Add("engine", p.Engine)

	var partialResponseValue string
	switch p.PartialResponseStrategy {
	case storepb.PartialResponseStrategy_WARN:
		partialResponseValue = strconv.FormatBool(true)
	case storepb.PartialResponseStrategy_ABORT:
		partialResponseValue = strconv.FormatBool(false)
	default:
		return errors.Errorf("unknown partial response strategy %v", p.PartialResponseStrategy)
	}

	// TODO(bwplotka): Apply change from bool to strategy in Query API as well.
	values.Add("partial_response", partialResponseValue)

	return nil
}

type Explanation struct {
	Name     string         `json:"name"`
	Children []*Explanation `json:"children,omitempty"`
}

// QueryInstant performs an instant query using a default HTTP client and returns results in model.Vector type.
func (c *Client) QueryInstant(ctx context.Context, base *url.URL, query string, t time.Time, opts QueryOptions) (model.Vector, []string, *Explanation, error) {
	params, err := url.ParseQuery(base.RawQuery)
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "parse raw query %s", base.RawQuery)
	}
	params.Add("query", query)
	params.Add("time", t.Format(time.RFC3339Nano))
	if !opts.DoNotAddThanosParams {
		if err := opts.AddTo(params); err != nil {
			return nil, nil, nil, errors.Wrap(err, "add thanos opts query params")
		}
	}

	u := *base
	u.Path = path.Join(u.Path, "/api/v1/query")
	u.RawQuery = params.Encode()

	level.Debug(c.logger).Log("msg", "querying instant", "url", u.String())

	span, ctx := tracing.StartSpan(ctx, "/prom_query_instant HTTP[client]")
	defer span.Finish()

	method := opts.Method
	if method == "" {
		method = http.MethodGet
	}

	body, _, err := c.req2xx(ctx, &u, method, opts.HTTPHeaders)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "read query instant response")
	}

	// Decode only ResultType and load Result only as RawJson since we don't know
	// structure of the Result yet.
	var m struct {
		Data struct {
			ResultType  string          `json:"resultType"`
			Result      json.RawMessage `json:"result"`
			Explanation *Explanation    `json:"explanation,omitempty"`
		} `json:"data"`

		Error     string `json:"error,omitempty"`
		ErrorType string `json:"errorType,omitempty"`
		// Extra fields supported by Thanos Querier.
		Warnings []string `json:"warnings"`
	}

	if err = json.Unmarshal(body, &m); err != nil {
		return nil, nil, nil, errors.Wrap(err, "unmarshal query instant response")
	}

	var vectorResult model.Vector

	// Decode the Result depending on the ResultType
	// Currently only `vector` and `scalar` types are supported.
	switch m.Data.ResultType {
	case string(parser.ValueTypeVector):
		if err = json.Unmarshal(m.Data.Result, &vectorResult); err != nil {
			return nil, nil, nil, errors.Wrap(err, "decode result into ValueTypeVector")
		}
	case string(parser.ValueTypeScalar):
		vectorResult, err = convertScalarJSONToVector(m.Data.Result)
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "decode result into ValueTypeScalar")
		}
	default:
		if m.Warnings != nil {
			return nil, nil, nil, errors.Errorf("error: %s, type: %s, warning: %s", m.Error, m.ErrorType, strings.Join(m.Warnings, ", "))
		}
		if m.Error != "" {
			return nil, nil, nil, errors.Errorf("error: %s, type: %s", m.Error, m.ErrorType)
		}
		return nil, nil, nil, errors.Errorf("received status code: 200, unknown response type: '%q'", m.Data.ResultType)
	}

	return vectorResult, m.Warnings, m.Data.Explanation, nil
}

// PromqlQueryInstant performs instant query and returns results in promql.Vector type that is compatible with promql package.
func (c *Client) PromqlQueryInstant(ctx context.Context, base *url.URL, query string, t time.Time, opts QueryOptions) (promql.Vector, []string, error) {
	vectorResult, warnings, _, err := c.QueryInstant(ctx, base, query, t, opts)
	if err != nil {
		return nil, nil, err
	}

	vec := make(promql.Vector, 0, len(vectorResult))

	b := labels.NewScratchBuilder(0)
	for _, e := range vectorResult {
		b.Reset()

		for k, v := range e.Metric {
			b.Add(string(k), string(v))
		}
		// TODO(mhoffm): shouldn't labels from prometheus results be already sorted?
		b.Sort()

		vec = append(vec, promql.Sample{
			Metric: b.Labels(),
			T:      int64(e.Timestamp),
			F:      float64(e.Value),
		})
	}

	return vec, warnings, nil
}

// QueryRange performs a range query using a default HTTP client and returns results in model.Matrix type.
func (c *Client) QueryRange(ctx context.Context, base *url.URL, query string, startTime, endTime, step int64, opts QueryOptions) (model.Matrix, []string, *Explanation, error) {
	params, err := url.ParseQuery(base.RawQuery)
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "parse raw query %s", base.RawQuery)
	}
	params.Add("query", query)
	params.Add("start", formatTime(timestamp.Time(startTime)))
	params.Add("end", formatTime(timestamp.Time(endTime)))
	params.Add("step", strconv.FormatInt(step, 10))
	if !opts.DoNotAddThanosParams {
		if err := opts.AddTo(params); err != nil {
			return nil, nil, nil, errors.Wrap(err, "add thanos opts query params")
		}
	}

	u := *base
	u.Path = path.Join(u.Path, "/api/v1/query_range")
	u.RawQuery = params.Encode()

	level.Debug(c.logger).Log("msg", "range query", "url", u.String())

	span, ctx := tracing.StartSpan(ctx, "/prom_query_range HTTP[client]")
	defer span.Finish()

	body, _, err := c.req2xx(ctx, &u, http.MethodGet, opts.HTTPHeaders)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "read query range response")
	}

	// Decode only ResultType and load Result only as RawJson since we don't know
	// structure of the Result yet.
	var m struct {
		Data struct {
			ResultType  string          `json:"resultType"`
			Result      json.RawMessage `json:"result"`
			Explanation *Explanation    `json:"explanation,omitempty"`
		} `json:"data"`

		Error     string `json:"error,omitempty"`
		ErrorType string `json:"errorType,omitempty"`
		// Extra fields supported by Thanos Querier.
		Warnings []string `json:"warnings"`
	}

	if err = json.Unmarshal(body, &m); err != nil {
		return nil, nil, nil, errors.Wrap(err, "unmarshal query range response")
	}

	var matrixResult model.Matrix

	// Decode the Result depending on the ResultType
	switch m.Data.ResultType {
	case string(parser.ValueTypeMatrix):
		if err = json.Unmarshal(m.Data.Result, &matrixResult); err != nil {
			return nil, nil, nil, errors.Wrap(err, "decode result into ValueTypeMatrix")
		}
	default:
		if m.Warnings != nil {
			return nil, nil, nil, errors.Errorf("error: %s, type: %s, warning: %s", m.Error, m.ErrorType, strings.Join(m.Warnings, ", "))
		}
		if m.Error != "" {
			return nil, nil, nil, errors.Errorf("error: %s, type: %s", m.Error, m.ErrorType)
		}

		return nil, nil, nil, errors.Errorf("received status code: 200, unknown response type: '%q'", m.Data.ResultType)
	}
	return matrixResult, m.Warnings, m.Data.Explanation, nil
}

// Scalar response consists of array with mixed types so it needs to be
// unmarshaled separately.
func convertScalarJSONToVector(scalarJSONResult json.RawMessage) (model.Vector, error) {
	var (
		// Do not specify exact length of the expected slice since JSON unmarshaling
		// would make the length fit the size and we won't be able to check the length afterwards.
		resultPointSlice []json.RawMessage
		resultTime       model.Time
		resultValue      model.SampleValue
	)
	if err := json.Unmarshal(scalarJSONResult, &resultPointSlice); err != nil {
		return nil, err
	}
	if len(resultPointSlice) != 2 {
		return nil, errors.Errorf("invalid scalar result format %v, expected timestamp -> value tuple", resultPointSlice)
	}
	if err := json.Unmarshal(resultPointSlice[0], &resultTime); err != nil {
		return nil, errors.Wrapf(err, "unmarshaling scalar time from %v", resultPointSlice)
	}
	if err := json.Unmarshal(resultPointSlice[1], &resultValue); err != nil {
		return nil, errors.Wrapf(err, "unmarshaling scalar value from %v", resultPointSlice)
	}
	return model.Vector{&model.Sample{
		Metric:    model.Metric{},
		Value:     resultValue,
		Timestamp: resultTime}}, nil
}

// AlertmanagerAlerts returns alerts from Alertmanager.
func (c *Client) AlertmanagerAlerts(ctx context.Context, base *url.URL) ([]*model.Alert, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/alerts")

	level.Debug(c.logger).Log("msg", "querying instant", "url", u.String())

	span, ctx := tracing.StartSpan(ctx, "/alertmanager_alerts HTTP[client]")
	defer span.Finish()

	body, _, err := c.req2xx(ctx, &u, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}

	// Decode only ResultType and load Result only as RawJson since we don't know
	// structure of the Result yet.
	var v struct {
		Data []*model.Alert `json:"data"`
	}
	if err = json.Unmarshal(body, &v); err != nil {
		return nil, errors.Wrap(err, "unmarshal alertmanager alert API response")
	}
	sort.Slice(v.Data, func(i, j int) bool {
		return v.Data[i].Labels.Before(v.Data[j].Labels)
	})
	return v.Data, nil
}

// BuildVersion returns Prometheus version from /api/v1/status/buildinfo Prometheus endpoint.
// For Prometheus versions < 2.14.0 it returns "0" as Prometheus version.
func (c *Client) BuildVersion(ctx context.Context, base *url.URL) (string, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/status/buildinfo")

	level.Debug(c.logger).Log("msg", "build version", "url", u.String())

	span, ctx := tracing.StartSpan(ctx, "/prom_buildversion HTTP[client]")
	defer span.Finish()

	// We get status code 404 or 405 for prometheus versions lower than 2.14.0
	body, code, err := c.req2xx(ctx, &u, http.MethodGet, nil)
	if err != nil {
		if code == http.StatusNotFound {
			return "0", nil
		}
		if code == http.StatusMethodNotAllowed {
			return "0", nil
		}
		return "", err
	}

	var b struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}

	if err = json.Unmarshal(body, &b); err != nil {
		return "", errors.Wrap(err, "unmarshal build info API response")
	}

	return b.Data.Version, nil
}

// LowestTimestamp returns the lowest timestamp in the TSDB by parsing the /metrics endpoint
// and extracting the prometheus_tsdb_lowest_timestamp_seconds metric from it.
func (c *Client) LowestTimestamp(ctx context.Context, base *url.URL) (int64, error) {
	u := *base
	u.Path = path.Join(u.Path, "/metrics")

	level.Debug(c.logger).Log("msg", "lowest timestamp", "url", u.String())

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, errors.Wrap(err, "create request")
	}

	span, ctx := tracing.StartSpan(ctx, "/lowest_timestamp HTTP[client]")
	defer span.Finish()

	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return 0, errors.Wrapf(err, "request metric against %s", u.String())
	}
	defer runutil.ExhaustCloseWithLogOnErr(c.logger, resp.Body, "request body")

	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return 0, errors.Wrapf(err, "parsing metric families against %s", u.String())
	}
	mf, ok := families["prometheus_tsdb_lowest_timestamp_seconds"]
	if !ok {
		return 0, errors.Wrapf(err, "metric families did not contain 'prometheus_tsdb_lowest_timestamp_seconds'")
	}
	val := 1000 * mf.GetMetric()[0].GetGauge().GetValue()

	// in the case that we dont have cut a block yet, TSDB lowest timestamp is math.MaxInt64
	// but its represented as float and truncated so we need to do this weird comparison.
	// Since we use this for fan-out pruning we use min timestamp here to include this prometheus.
	if val == float64(math.MaxInt64) {
		return math.MinInt64, nil
	}
	return int64(val), nil
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.Unix())+float64(t.Nanosecond())/1e9, 'f', -1, 64)
}

func (c *Client) get2xxResultWithGRPCErrors(ctx context.Context, spanName string, u *url.URL, data interface{}) error {
	span, ctx := tracing.StartSpan(ctx, spanName)
	defer span.Finish()

	body, code, err := c.req2xx(ctx, u, http.MethodGet, nil)
	if err != nil {
		if code, exists := statusToCode[code]; exists && code != 0 {
			return status.Error(code, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}

	if code == http.StatusNoContent {
		return nil
	}

	var m struct {
		Data   interface{} `json:"data"`
		Status string      `json:"status"`
		Error  string      `json:"error"`
	}

	if err = json.Unmarshal(body, &m); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	if m.Status != SUCCESS {
		code, exists := statusToCode[code]
		if !exists {
			return status.Error(codes.Internal, m.Error)
		}
		return status.Error(code, m.Error)
	}

	if err = json.Unmarshal(body, &data); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	return nil
}

// SeriesInGRPC returns the labels from Prometheus series API. It uses gRPC errors.
// NOTE: This method is tested in pkg/store/prometheus_test.go against Prometheus.
func (c *Client) SeriesInGRPC(ctx context.Context, base *url.URL, matchers []*labels.Matcher, startTime, endTime int64, limit int) ([]map[string]string, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/series")
	q := u.Query()

	q.Add("match[]", storepb.PromMatchersToString(matchers...))
	q.Add("start", formatTime(timestamp.Time(startTime)))
	q.Add("end", formatTime(timestamp.Time(endTime)))
	if limit > 0 {
		q.Add("limit", strconv.Itoa(limit))
	}
	u.RawQuery = q.Encode()

	var m struct {
		Data []map[string]string `json:"data"`
	}

	return m.Data, c.get2xxResultWithGRPCErrors(ctx, "/prom_series HTTP[client]", &u, &m)
}

// LabelNamesInGRPC returns all known label names constrained by the given matchers. It uses gRPC errors.
// NOTE: This method is tested in pkg/store/prometheus_test.go against Prometheus.
func (c *Client) LabelNamesInGRPC(ctx context.Context, base *url.URL, matchers []*labels.Matcher, startTime, endTime int64, limit int) ([]string, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/labels")
	q := u.Query()

	if len(matchers) > 0 {
		q.Add("match[]", storepb.PromMatchersToString(matchers...))
	}
	q.Add("start", formatTime(timestamp.Time(startTime)))
	q.Add("end", formatTime(timestamp.Time(endTime)))
	if limit > 0 {
		q.Add("limit", strconv.Itoa(limit))
	}
	u.RawQuery = q.Encode()

	var m struct {
		Data []string `json:"data"`
	}
	return m.Data, c.get2xxResultWithGRPCErrors(ctx, "/prom_label_names HTTP[client]", &u, &m)
}

// LabelValuesInGRPC returns all known label values for a given label name. It uses gRPC errors.
// NOTE: This method is tested in pkg/store/prometheus_test.go against Prometheus.
func (c *Client) LabelValuesInGRPC(ctx context.Context, base *url.URL, label string, matchers []*labels.Matcher, startTime, endTime int64, limit int) ([]string, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/label/", label, "/values")
	q := u.Query()

	if len(matchers) > 0 {
		q.Add("match[]", storepb.PromMatchersToString(matchers...))
	}
	q.Add("start", formatTime(timestamp.Time(startTime)))
	q.Add("end", formatTime(timestamp.Time(endTime)))
	if limit > 0 {
		q.Add("limit", strconv.Itoa(limit))
	}
	u.RawQuery = q.Encode()

	var m struct {
		Data []string `json:"data"`
	}
	return m.Data, c.get2xxResultWithGRPCErrors(ctx, "/prom_label_values HTTP[client]", &u, &m)
}

// RulesInGRPC returns the rules from Prometheus rules API. It uses gRPC errors.
// NOTE: This method is tested in pkg/store/prometheus_test.go against Prometheus.
func (c *Client) RulesInGRPC(ctx context.Context, base *url.URL, typeRules string) ([]*rulespb.RuleGroup, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/rules")

	if typeRules != "" {
		q := u.Query()
		q.Add("type", typeRules)
		u.RawQuery = q.Encode()
	}

	var m struct {
		Data *rulespb.RuleGroups `json:"data"`
	}

	if err := c.get2xxResultWithGRPCErrors(ctx, "/prom_rules HTTP[client]", &u, &m); err != nil {
		return nil, err
	}

	// Prometheus does not support PartialResponseStrategy, and probably would never do. Make it Abort by default.
	for _, g := range m.Data.Groups {
		g.PartialResponseStrategy = storepb.PartialResponseStrategy_ABORT
	}
	return m.Data.Groups, nil
}

// AlertsInGRPC returns the rules from Prometheus alerts API. It uses gRPC errors.
// NOTE: This method is tested in pkg/store/prometheus_test.go against Prometheus.
func (c *Client) AlertsInGRPC(ctx context.Context, base *url.URL) ([]*rulespb.AlertInstance, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/alerts")

	var m struct {
		Data struct {
			Alerts []*rulespb.AlertInstance `json:"alerts"`
		} `json:"data"`
	}

	if err := c.get2xxResultWithGRPCErrors(ctx, "/prom_alerts HTTP[client]", &u, &m); err != nil {
		return nil, err
	}

	// Prometheus does not support PartialResponseStrategy, and probably would never do. Make it Abort by default.
	for _, g := range m.Data.Alerts {
		g.PartialResponseStrategy = storepb.PartialResponseStrategy_ABORT
	}
	return m.Data.Alerts, nil
}

// MetricMetadataInGRPC returns the metadata from Prometheus metric metadata API. It uses gRPC errors.
func (c *Client) MetricMetadataInGRPC(ctx context.Context, base *url.URL, metric string, limit int) (map[string][]metadatapb.Meta, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/metadata")
	q := u.Query()

	if metric != "" {
		q.Add("metric", metric)
	}
	if limit >= 0 {
		q.Add("limit", strconv.Itoa(limit))
	}

	u.RawQuery = q.Encode()

	var v struct {
		Data map[string][]metadatapb.Meta `json:"data"`
	}
	return v.Data, c.get2xxResultWithGRPCErrors(ctx, "/prom_metric_metadata HTTP[client]", &u, &v)
}

// ExemplarsInGRPC returns the exemplars from Prometheus exemplars API. It uses gRPC errors.
// NOTE: This method is tested in pkg/store/prometheus_test.go against Prometheus.
func (c *Client) ExemplarsInGRPC(ctx context.Context, base *url.URL, query string, startTime, endTime int64) ([]*exemplarspb.ExemplarData, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/query_exemplars")
	q := u.Query()

	q.Add("query", query)
	q.Add("start", formatTime(timestamp.Time(startTime)))
	q.Add("end", formatTime(timestamp.Time(endTime)))
	u.RawQuery = q.Encode()

	var m struct {
		Data []*exemplarspb.ExemplarData `json:"data"`
	}

	if err := c.get2xxResultWithGRPCErrors(ctx, "/prom_exemplars HTTP[client]", &u, &m); err != nil {
		return nil, err
	}

	return m.Data, nil
}

func (c *Client) TargetsInGRPC(ctx context.Context, base *url.URL, stateTargets string) (*targetspb.TargetDiscovery, error) {
	u := *base
	u.Path = path.Join(u.Path, "/api/v1/targets")

	if stateTargets != "" {
		q := u.Query()
		q.Add("state", stateTargets)
		u.RawQuery = q.Encode()
	}

	var v struct {
		Data *targetspb.TargetDiscovery `json:"data"`
	}
	return v.Data, c.get2xxResultWithGRPCErrors(ctx, "/prom_targets HTTP[client]", &u, &v)
}
