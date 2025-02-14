// Copyright (c) The Cortex Authors.
// Licensed under the Apache License 2.0.

package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/hashicorp/golang-lru"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	querier_stats "github.com/thanos-io/thanos/internal/cortex/querier/stats"
	"github.com/thanos-io/thanos/internal/cortex/tenant"
	"github.com/thanos-io/thanos/internal/cortex/util"
	util_log "github.com/thanos-io/thanos/internal/cortex/util/log"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/httpgrpc/server"
)

const (
	// StatusClientClosedRequest is the status code for when a client request cancellation of an http request
	StatusClientClosedRequest = 499
	ServiceTimingHeaderName   = "Server-Timing"
)

var (
	errCanceled              = httpgrpc.Errorf(StatusClientClosedRequest, context.Canceled.Error())
	errDeadlineExceeded      = httpgrpc.Errorf(http.StatusGatewayTimeout, context.DeadlineExceeded.Error())
	errRequestEntityTooLarge = httpgrpc.Errorf(http.StatusRequestEntityTooLarge, "http: request body too large")
	cacheableResponseCodes   = []int{http.StatusRequestTimeout, http.StatusGatewayTimeout, http.StatusBadRequest}
)

// HandlerConfig Config for a Handler.
type HandlerConfig struct {
	LogQueriesLongerThan     time.Duration `yaml:"log_queries_longer_than"`
	MaxBodySize              int64         `yaml:"max_body_size"`
	QueryStatsEnabled        bool          `yaml:"query_stats_enabled"`
	LogFailedQueries         bool          `yaml:"log_failed_queries"`
	FailedQueryCacheCapacity int           `yaml:"failed_query_cache_capacity"`
}

// Handler accepts queries and forwards them to RoundTripper. It can log slow queries,
// but all other logic is inside the RoundTripper.
type Handler struct {
	cfg          HandlerConfig
	log          log.Logger
	roundTripper http.RoundTripper
	lruCache     *lru.Cache
	regex        *regexp.Regexp
	errorExtract *regexp.Regexp

	// Metrics.
	querySeconds *prometheus.CounterVec
	querySeries  *prometheus.CounterVec
	queryBytes   *prometheus.CounterVec
	cachedHits   prometheus.Counter
	activeUsers  *util.ActiveUsersCleanupService
}

// NewHandler creates a new frontend handler.
func NewHandler(cfg HandlerConfig, roundTripper http.RoundTripper, log log.Logger, reg prometheus.Registerer) http.Handler {
	var (
		LruCache *lru.Cache
		err      error
	)

	if cfg.FailedQueryCacheCapacity > 0 {
		LruCache, err = lru.New(cfg.FailedQueryCacheCapacity)
		if err != nil {
			LruCache = nil
			level.Warn(log).Log("msg", "Failed to create LruCache", "error", err)
		}
	}

	h := &Handler{
		cfg:          cfg,
		log:          log,
		roundTripper: roundTripper,
		lruCache:     LruCache,
		regex:        regexp.MustCompile(`[\s\n\t]+`),
		errorExtract: regexp.MustCompile(`Code\((\d+)\)`),
	}

	if cfg.QueryStatsEnabled {
		h.querySeconds = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_query_seconds_total",
			Help: "Total amount of wall clock time spend processing queries.",
		}, []string{"user"})

		h.querySeries = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_query_fetched_series_total",
			Help: "Number of series fetched to execute a query.",
		}, []string{"user"})

		h.queryBytes = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_query_fetched_chunks_bytes_total",
			Help: "Size of all chunks fetched to execute a query in bytes.",
		}, []string{"user"})

		h.activeUsers = util.NewActiveUsersCleanupWithDefaultValues(func(user string) {
			h.querySeconds.DeleteLabelValues(user)
			h.querySeries.DeleteLabelValues(user)
			h.queryBytes.DeleteLabelValues(user)
		})

		// If cleaner stops or fail, we will simply not clean the metrics for inactive users.
		_ = h.activeUsers.StartAsync(context.Background())
	}

	h.cachedHits = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "cached_failed_queries_count",
		Help: "Total number of queries that hit the failed query cache.",
	})

	return h
}

func (f *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var (
		stats                      *querier_stats.Stats
		queryString                url.Values
		queryExpressionNormalized  string
		queryExpressionRangeLength int
	)

	// Initialise the stats in the context and make sure it's propagated
	// down the request chain.
	if f.cfg.QueryStatsEnabled {
		var ctx context.Context
		stats, ctx = querier_stats.ContextWithEmptyStats(r.Context())
		r = r.WithContext(ctx)
	}

	defer func() {
		_ = r.Body.Close()
	}()

	// Buffer the body for later use to track slow queries.
	var buf bytes.Buffer
	r.Body = http.MaxBytesReader(w, r.Body, f.cfg.MaxBodySize)
	r.Body = io.NopCloser(io.TeeReader(r.Body, &buf))

	// Check if caching is enabled.
	if f.lruCache != nil {
		// Store query expression.
		queryExpressionNormalized = f.regex.ReplaceAllString(r.URL.Query().Get("query"), " ")

		// Store query time range length.
		queryExpressionRangeLength = getQueryRangeSeconds(r)

		// Check if query in cache and whether value exceeds time range length.
		if value, ok := f.lruCache.Get(queryExpressionNormalized); ok && value.(int) >= queryExpressionRangeLength {
			w.WriteHeader(http.StatusForbidden)
			level.Info(util_log.WithContext(r.Context(), f.log)).Log(
				"msg", "Retrieved query from cache",
				"normalized_query", queryExpressionNormalized,
				"range_seconds", queryExpressionRangeLength,
			)
			f.cachedHits.Inc()
			return
		}
	}

	startTime := time.Now()
	resp, err := f.roundTripper.RoundTrip(r)
	queryResponseTime := time.Since(startTime)

	if err != nil {
		writeError(w, err)
		queryString = f.parseRequestQueryString(r, buf)

		// Check if caching is enabled.
		if f.lruCache != nil {
			f.updateFailedQueryCache(err, queryExpressionNormalized, queryExpressionRangeLength, r)
		}

		if f.cfg.LogFailedQueries {
			f.reportFailedQuery(r, queryString, err)
		}
		return
	}

	hs := w.Header()
	for h, vs := range resp.Header {
		hs[h] = vs
	}

	if f.cfg.QueryStatsEnabled {
		writeServiceTimingHeader(queryResponseTime, hs, stats)
	}

	w.WriteHeader(resp.StatusCode)
	// log copy response body error so that we will know even though success response code returned
	bytesCopied, err := io.Copy(w, resp.Body)
	if err != nil && !errors.Is(err, syscall.EPIPE) {
		level.Error(util_log.WithContext(r.Context(), f.log)).Log("msg", "write response body error", "bytesCopied", bytesCopied, "err", err)
	}

	// Check whether we should parse the query string.
	shouldReportSlowQuery := f.cfg.LogQueriesLongerThan != 0 && queryResponseTime > f.cfg.LogQueriesLongerThan
	if shouldReportSlowQuery || f.cfg.QueryStatsEnabled {
		queryString = f.parseRequestQueryString(r, buf)
	}

	if shouldReportSlowQuery {
		f.reportSlowQuery(r, hs, queryString, queryResponseTime)
	}
	if f.cfg.QueryStatsEnabled {
		f.reportQueryStats(r, queryString, queryResponseTime, stats)
	}
}

func (f *Handler) updateFailedQueryCache(err error, queryExpressionNormalized string, queryExpressionRangeLength int, r *http.Request) {
	// Extracting error code from error string.
	codeExtract := f.errorExtract.FindStringSubmatch(err.Error())

	// Checking if error code extracted successfully.
	if codeExtract == nil || len(codeExtract) < 2 {
		level.Error(util_log.WithContext(r.Context(), f.log)).Log(
			"msg", "Error string regex conversion error",
			"normalized_query", queryExpressionNormalized,
			"range_seconds", queryExpressionRangeLength,
			"error", err)
		return
	}

	// Converting error code to int.
	errCode, strConvError := strconv.Atoi(codeExtract[1])

	// Checking if error code extracted properly from string.
	if strConvError != nil {
		level.Error(util_log.WithContext(r.Context(), f.log)).Log(
			"msg", "String to int conversion error",
			"normalized_query", queryExpressionNormalized,
			"range_seconds", queryExpressionRangeLength,
			"error", err)
		return
	}

	// If error should be cached, store it in cache.
	if !isCacheableError(errCode) {
		level.Debug(util_log.WithContext(r.Context(), f.log)).Log(
			"msg", "Query not cached due to non-cacheable error code",
			"normalized_query", queryExpressionNormalized,
			"range_seconds", queryExpressionRangeLength,
			"error", err,
		)
		return
	}

	// Checks if queryExpression is already in cache, and updates time range length value to min of stored and new value.
	if contains, _ := f.lruCache.ContainsOrAdd(queryExpressionNormalized, queryExpressionRangeLength); contains {
		if oldValue, ok := f.lruCache.Get(queryExpressionNormalized); ok {
			queryExpressionRangeLength = min(queryExpressionRangeLength, oldValue.(int))
		}
		f.lruCache.Add(queryExpressionNormalized, queryExpressionRangeLength)
	}

	level.Debug(util_log.WithContext(r.Context(), f.log)).Log(
		"msg", "Cached a failed query",
		"normalized_query", queryExpressionNormalized,
		"range_seconds", queryExpressionRangeLength,
		"error", err,
	)

}

// isCacheableError Returns true if response code is in pre-defined cacheable errors list, else returns false.
func isCacheableError(statusCode int) bool {
	for _, errStatusCode := range cacheableResponseCodes {
		if errStatusCode == statusCode {
			return true
		}
	}
	return false
}

// Time range length for queries, if either of "start" or "end" are not present, return 0.
func getQueryRangeSeconds(r *http.Request) int {
	start, err := strconv.Atoi(r.URL.Query().Get("start"))
	if err != nil {
		return 0
	}
	end, err := strconv.Atoi(r.URL.Query().Get("end"))
	if err != nil {
		return 0
	}
	return end - start
}

func (f *Handler) reportFailedQuery(r *http.Request, queryString url.Values, err error) {
	// NOTE(GiedriusS): see https://github.com/grafana/grafana/pull/60301 for more info.
	grafanaDashboardUID := "-"
	if dashboardUID := r.Header.Get("X-Dashboard-Uid"); dashboardUID != "" {
		grafanaDashboardUID = dashboardUID
	}
	grafanaPanelID := "-"
	if panelID := r.Header.Get("X-Panel-Id"); panelID != "" {
		grafanaPanelID = panelID
	}
	remoteUser, _, _ := r.BasicAuth()

	logMessage := append([]interface{}{
		"msg", "failed query",
		"method", r.Method,
		"host", r.Host,
		"path", r.URL.Path,
		"remote_user", remoteUser,
		"remote_addr", r.RemoteAddr,
		"error", err.Error(),
		"grafana_dashboard_uid", grafanaDashboardUID,
		"grafana_panel_id", grafanaPanelID,
	}, formatQueryString(queryString)...)

	level.Error(util_log.WithContext(r.Context(), f.log)).Log(logMessage...)
}

// reportSlowQuery reports slow queries.
func (f *Handler) reportSlowQuery(r *http.Request, responseHeaders http.Header, queryString url.Values, queryResponseTime time.Duration) {
	// NOTE(GiedriusS): see https://github.com/grafana/grafana/pull/60301 for more info.
	grafanaDashboardUID := "-"
	if dashboardUID := r.Header.Get("X-Dashboard-Uid"); dashboardUID != "" {
		grafanaDashboardUID = dashboardUID
	}
	grafanaPanelID := "-"
	if panelID := r.Header.Get("X-Panel-Id"); panelID != "" {
		grafanaPanelID = panelID
	}
	thanosTraceID := "-"
	if traceID := responseHeaders.Get("X-Thanos-Trace-Id"); traceID != "" {
		thanosTraceID = traceID
	}

	remoteUser, _, _ := r.BasicAuth()

	logMessage := append([]interface{}{
		"msg", "slow query detected",
		"method", r.Method,
		"host", r.Host,
		"path", r.URL.Path,
		"remote_user", remoteUser,
		"remote_addr", r.RemoteAddr,
		"time_taken", queryResponseTime.String(),
		"grafana_dashboard_uid", grafanaDashboardUID,
		"grafana_panel_id", grafanaPanelID,
		"trace_id", thanosTraceID,
	}, formatQueryString(queryString)...)

	level.Info(util_log.WithContext(r.Context(), f.log)).Log(logMessage...)
}

func (f *Handler) reportQueryStats(r *http.Request, queryString url.Values, queryResponseTime time.Duration, stats *querier_stats.Stats) {
	tenantIDs, err := tenant.TenantIDs(r.Context())
	if err != nil {
		return
	}
	userID := tenant.JoinTenantIDs(tenantIDs)
	wallTime := stats.LoadWallTime()
	numSeries := stats.LoadFetchedSeries()
	numBytes := stats.LoadFetchedChunkBytes()
	remoteUser, _, _ := r.BasicAuth()

	// Track stats.
	f.querySeconds.WithLabelValues(userID).Add(wallTime.Seconds())
	f.querySeries.WithLabelValues(userID).Add(float64(numSeries))
	f.queryBytes.WithLabelValues(userID).Add(float64(numBytes))
	f.activeUsers.UpdateUserTimestamp(userID, time.Now())

	// Log stats.
	logMessage := append([]interface{}{
		"msg", "query stats",
		"component", "query-frontend",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_user", remoteUser,
		"remote_addr", r.RemoteAddr,
		"response_time", queryResponseTime,
		"query_wall_time_seconds", wallTime.Seconds(),
		"fetched_series_count", numSeries,
		"fetched_chunks_bytes", numBytes,
	}, formatQueryString(queryString)...)

	level.Info(util_log.WithContext(r.Context(), f.log)).Log(logMessage...)
}

func (f *Handler) parseRequestQueryString(r *http.Request, bodyBuf bytes.Buffer) url.Values {
	// Use previously buffered body.
	r.Body = io.NopCloser(&bodyBuf)

	// Ensure the form has been parsed so all the parameters are present
	err := r.ParseForm()
	if err != nil {
		level.Warn(util_log.WithContext(r.Context(), f.log)).Log("msg", "unable to parse request form", "err", err)
		return nil
	}

	return r.Form
}

func formatQueryString(queryString url.Values) (fields []interface{}) {
	for k, v := range queryString {
		fields = append(fields, fmt.Sprintf("param_%s", k), strings.Join(v, ","))
	}
	return fields
}

func writeError(w http.ResponseWriter, err error) {
	switch err {
	case context.Canceled:
		err = errCanceled
	case context.DeadlineExceeded:
		err = errDeadlineExceeded
	default:
		if util.IsRequestBodyTooLarge(err) {
			err = errRequestEntityTooLarge
		}
	}
	server.WriteError(w, err)
}

func writeServiceTimingHeader(queryResponseTime time.Duration, headers http.Header, stats *querier_stats.Stats) {
	if stats != nil {
		parts := make([]string, 0)
		parts = append(parts, statsValue("querier_wall_time", stats.LoadWallTime()))
		parts = append(parts, statsValue("response_time", queryResponseTime))
		headers.Set(ServiceTimingHeaderName, strings.Join(parts, ", "))
	}
}

func statsValue(name string, d time.Duration) string {
	durationInMs := strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', -1, 64)
	return name + ";dur=" + durationInMs
}
