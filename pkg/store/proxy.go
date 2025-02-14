// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/model/labels"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/info/infopb"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/strutil"
	"github.com/thanos-io/thanos/pkg/tenancy"
	"github.com/thanos-io/thanos/pkg/tracing"
)

type ctxKey int

// UninitializedTSDBTime is the TSDB start time of an uninitialized TSDB instance.
const UninitializedTSDBTime = math.MaxInt64

// StoreMatcherKey is the context key for the store's allow list.
const StoreMatcherKey = ctxKey(0)

// ErrorNoStoresMatched is returned if the query does not match any data.
// This can happen with Query servers trees and external labels.
var ErrorNoStoresMatched = errors.New("No StoreAPIs matched for this query")

// Client holds meta information about a store.
type Client interface {
	// StoreClient to access the store.
	storepb.StoreClient

	// LabelSets that each apply to some data exposed by the backing store.
	LabelSets() []labels.Labels

	// TimeRange returns minimum and maximum time range of data in the store.
	TimeRange() (mint int64, maxt int64)

	// TSDBInfos returns metadata about each TSDB backed by the client.
	TSDBInfos() []infopb.TSDBInfo

	// SupportsSharding returns true if sharding is supported by the underlying store.
	SupportsSharding() bool

	// SupportsWithoutReplicaLabels returns true if trimming replica labels
	// and sorted response is supported by the underlying store.
	SupportsWithoutReplicaLabels() bool

	// String returns the string representation of the store client.
	String() string

	// Addr returns address of the store client. If second parameter is true, the client
	// represents a local client (server-as-client) and has no remote address.
	Addr() (addr string, isLocalClient bool)

	// A replica key defines a set of endpoints belong to the same replica.
	// E.g, "pantheon-db-rep0", "pantheon-db-rep1", "long-range-store".
	ReplicaKey() string
	// A group key defeines a group of replicas that belong to the same group.
	// E.g. "pantheon-db" has replicas "pantheon-db-rep0", "pantheon-db-rep1".
	//		"long-range-store" has only one replica, "long-range-store".
	GroupKey() string
}

// ProxyStore implements the store API that proxies request to all given underlying stores.
type ProxyStore struct {
	logger         log.Logger
	stores         func() []Client
	component      component.StoreAPI
	selectorLabels labels.Labels
	buffers        sync.Pool

	responseTimeout   time.Duration
	metrics           *proxyStoreMetrics
	retrievalStrategy RetrievalStrategy
	debugLogging      bool
	tsdbSelector      *TSDBSelector
}

type proxyStoreMetrics struct {
	emptyStreamResponses prometheus.Counter
}

func newProxyStoreMetrics(reg prometheus.Registerer) *proxyStoreMetrics {
	var m proxyStoreMetrics

	m.emptyStreamResponses = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_proxy_store_empty_stream_responses_total",
		Help: "Total number of empty responses received.",
	})

	return &m
}

func RegisterStoreServer(storeSrv storepb.StoreServer, logger log.Logger) func(*grpc.Server) {
	return func(s *grpc.Server) {
		storepb.RegisterStoreServer(s, NewRecoverableStoreServer(logger, storeSrv))
	}
}

// BucketStoreOption are functions that configure BucketStore.
type ProxyStoreOption func(s *ProxyStore)

// WithProxyStoreDebugLogging toggles debug logging.
func WithProxyStoreDebugLogging(enable bool) ProxyStoreOption {
	return func(s *ProxyStore) {
		s.debugLogging = enable
	}
}

// WithTSDBSelector sets the TSDB selector for the proxy.
func WithTSDBSelector(selector *TSDBSelector) ProxyStoreOption {
	return func(s *ProxyStore) {
		s.tsdbSelector = selector
	}
}

// NewProxyStore returns a new ProxyStore that uses the given clients that implements storeAPI to fan-in all series to the client.
// Note that there is no deduplication support. Deduplication should be done on the highest level (just before PromQL).
func NewProxyStore(
	logger log.Logger,
	reg prometheus.Registerer,
	stores func() []Client,
	component component.StoreAPI,
	selectorLabels labels.Labels,
	responseTimeout time.Duration,
	retrievalStrategy RetrievalStrategy,
	options ...ProxyStoreOption,
) *ProxyStore {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	metrics := newProxyStoreMetrics(reg)
	s := &ProxyStore{
		logger:         logger,
		stores:         stores,
		component:      component,
		selectorLabels: selectorLabels,
		buffers: sync.Pool{New: func() interface{} {
			b := make([]byte, 0, initialBufSize)
			return &b
		}},
		responseTimeout:   responseTimeout,
		metrics:           metrics,
		retrievalStrategy: retrievalStrategy,
		tsdbSelector:      DefaultSelector,
	}

	for _, option := range options {
		option(s)
	}

	return s
}

// Info returns store information about the external labels this store have.
func (s *ProxyStore) Info(_ context.Context, _ *storepb.InfoRequest) (*storepb.InfoResponse, error) {
	res := &storepb.InfoResponse{
		StoreType: s.component.ToProto(),
		Labels:    labelpb.ZLabelsFromPromLabels(s.selectorLabels),
	}

	minTime := int64(math.MaxInt64)
	maxTime := int64(0)
	stores := s.stores()

	// Edge case: we have no data if there are no stores.
	if len(stores) == 0 {
		res.MaxTime = 0
		res.MinTime = 0

		return res, nil
	}

	for _, s := range stores {
		mint, maxt := s.TimeRange()
		if mint < minTime {
			minTime = mint
		}
		if maxt > maxTime {
			maxTime = maxt
		}
	}

	res.MaxTime = maxTime
	res.MinTime = minTime

	labelSets := make(map[uint64]labelpb.ZLabelSet, len(stores))
	for _, st := range stores {
		for _, lset := range st.LabelSets() {
			mergedLabelSet := labelpb.ExtendSortedLabels(lset, s.selectorLabels)
			labelSets[mergedLabelSet.Hash()] = labelpb.ZLabelSet{Labels: labelpb.ZLabelsFromPromLabels(mergedLabelSet)}
		}
	}

	res.LabelSets = make([]labelpb.ZLabelSet, 0, len(labelSets))
	for _, v := range labelSets {
		res.LabelSets = append(res.LabelSets, v)
	}

	// We always want to enforce announcing the subset of data that
	// selector-labels represents. If no label-sets are announced by the
	// store-proxy's discovered stores, then we still want to enforce
	// announcing this subset by announcing the selector as the label-set.
	if len(res.LabelSets) == 0 && len(res.Labels) > 0 {
		res.LabelSets = append(res.LabelSets, labelpb.ZLabelSet{Labels: res.Labels})
	}

	return res, nil
}

func (s *ProxyStore) LabelSet() []labelpb.ZLabelSet {
	stores := s.stores()
	if len(stores) == 0 {
		return []labelpb.ZLabelSet{}
	}

	mergedLabelSets := make(map[uint64]labelpb.ZLabelSet, len(stores))
	for _, st := range stores {
		for _, lset := range st.LabelSets() {
			mergedLabelSet := labelpb.ExtendSortedLabels(lset, s.selectorLabels)
			mergedLabelSets[mergedLabelSet.Hash()] = labelpb.ZLabelSet{Labels: labelpb.ZLabelsFromPromLabels(mergedLabelSet)}
		}
	}

	labelSets := make([]labelpb.ZLabelSet, 0, len(mergedLabelSets))
	for _, v := range mergedLabelSets {
		labelSets = append(labelSets, v)
	}

	// We always want to enforce announcing the subset of data that
	// selector-labels represents. If no label-sets are announced by the
	// store-proxy's discovered stores, then we still want to enforce
	// announcing this subset by announcing the selector as the label-set.
	selectorLabels := labelpb.ZLabelsFromPromLabels(s.selectorLabels)
	if len(labelSets) == 0 && len(selectorLabels) > 0 {
		labelSets = append(labelSets, labelpb.ZLabelSet{Labels: selectorLabels})
	}

	return labelSets
}

func (s *ProxyStore) TimeRange() (int64, int64) {
	stores := s.stores()
	if len(stores) == 0 {
		return math.MinInt64, math.MaxInt64
	}

	var minTime, maxTime int64 = math.MaxInt64, math.MinInt64
	for _, s := range stores {
		storeMinTime, storeMaxTime := s.TimeRange()
		if storeMinTime < minTime {
			minTime = storeMinTime
		}
		if storeMaxTime > maxTime {
			maxTime = storeMaxTime
		}
	}

	return minTime, maxTime
}

func (s *ProxyStore) TSDBInfos() []infopb.TSDBInfo {
	infos := make([]infopb.TSDBInfo, 0)
	for _, st := range s.stores() {
		matches, _ := s.tsdbSelector.MatchLabelSets(st.LabelSets()...)
		if !matches {
			continue
		}
		infos = append(infos, st.TSDBInfos()...)
	}
	return infos
}

func (s *ProxyStore) Series(originalRequest *storepb.SeriesRequest, srv storepb.Store_SeriesServer) error {
	// TODO(bwplotka): This should be part of request logger, otherwise it does not make much sense. Also, could be
	// tiggered by tracing span to reduce cognitive load.
	reqLogger := log.With(s.logger, "component", "proxy")
	if s.debugLogging {
		reqLogger = log.With(reqLogger, "request", originalRequest.String())
	}

	match, matchers, err := matchesExternalLabels(originalRequest.Matchers, s.selectorLabels)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if !match {
		return nil
	}
	if len(matchers) == 0 {
		return status.Error(codes.InvalidArgument, errors.New("no matchers specified (excluding selector labels)").Error())
	}
	storeMatchers, _ := storepb.PromMatchersToMatchers(matchers...) // Error would be returned by matchesExternalLabels, so skip check.

	storeDebugMsgs := []string{}
	r := &storepb.SeriesRequest{
		MinTime:                 originalRequest.MinTime,
		MaxTime:                 originalRequest.MaxTime,
		Matchers:                storeMatchers,
		Aggregates:              originalRequest.Aggregates,
		MaxResolutionWindow:     originalRequest.MaxResolutionWindow,
		SkipChunks:              originalRequest.SkipChunks,
		QueryHints:              originalRequest.QueryHints,
		PartialResponseDisabled: originalRequest.PartialResponseDisabled,
		PartialResponseStrategy: originalRequest.PartialResponseStrategy,
		ShardInfo:               originalRequest.ShardInfo,
		WithoutReplicaLabels:    originalRequest.WithoutReplicaLabels,
	}

	// We may arrive here either via the promql engine
	// or as a result of a grpc call in layered queries
	ctx := srv.Context()
	tenant, foundTenant := tenancy.GetTenantFromGRPCMetadata(ctx)
	if !foundTenant {
		if ctx.Value(tenancy.TenantKey) != nil {
			tenant = ctx.Value(tenancy.TenantKey).(string)
		}
	}

	ctx = metadata.AppendToOutgoingContext(ctx, tenancy.DefaultTenantHeader, tenant)
	level.Debug(s.logger).Log("msg", "Tenant info in Series()", "tenant", tenant)

	var (
		stores         []Client
		storeLabelSets []labels.Labels
	)
	// groupReplicaStores[groupKey][replicaKey] = number of stores with the groupKey and replicaKey
	groupReplicaStores := make(map[string]map[string]int)
	// failedStores[groupKey][replicaKey] = number of store failures
	failedStores := make(map[string]map[string]int)
	totalFailedStores := 0
	bumpCounter := func(key1, key2 string, mp map[string]map[string]int) {
		if _, ok := mp[key1]; !ok {
			mp[key1] = make(map[string]int)
		}
		mp[key1][key2]++
	}

	for _, st := range s.stores() {
		// We might be able to skip the store if its meta information indicates it cannot have series matching our query.
		if ok, reason := storeMatches(ctx, st, s.debugLogging, originalRequest.MinTime, originalRequest.MaxTime, matchers...); !ok {
			if s.debugLogging {
				storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s filtered out due to: %v", st, reason))
			}
			continue
		}
		matches, extraMatchers := s.tsdbSelector.MatchLabelSets(st.LabelSets()...)
		if !matches {
			if s.debugLogging {
				storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s filtered out due to: %v", st, "tsdb selector"))
			}
			continue
		}
		storeLabelSets = append(storeLabelSets, extraMatchers...)

		stores = append(stores, st)
		bumpCounter(st.GroupKey(), st.ReplicaKey(), groupReplicaStores)
	}
	if len(stores) == 0 {
		level.Debug(reqLogger).Log("err", ErrorNoStoresMatched, "stores", strings.Join(storeDebugMsgs, ";"))
		return nil
	}
	r.Matchers = append(r.Matchers, MatchersForLabelSets(storeLabelSets)...)

	storeResponses := make([]respSet, 0, len(stores))

	checkGroupReplicaErrors := func(st Client, err error) error {
		if len(failedStores[st.GroupKey()]) > 1 {
			level.Error(reqLogger).Log(
				"msg", "Multipel replicas have failures for the same group",
				"group", st.GroupKey(),
				"replicas", failedStores[st.GroupKey()],
			)
			return err
		}
		if len(groupReplicaStores[st.GroupKey()]) == 1 && failedStores[st.GroupKey()][st.ReplicaKey()] > 1 {
			level.Error(reqLogger).Log(
				"msg", "A single replica group has multiple failures",
				"group", st.GroupKey(),
				"replicas", failedStores[st.GroupKey()],
			)
			return err
		}
		return nil
	}

	logGroupReplicaErrors := func() {
		if len(failedStores) > 0 {
			level.Warn(s.logger).Log("msg", "Group/replica errors",
				"errors", fmt.Sprintf("%+v", failedStores),
				"total_failed_stores", totalFailedStores,
			)
		}
	}
	defer logGroupReplicaErrors()

	for _, st := range stores {
		st := st
		if s.debugLogging {
			storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s queried", st))
		}

		respSet, err := newAsyncRespSet(ctx, st, r, s.responseTimeout, s.retrievalStrategy, &s.buffers, r.ShardInfo, reqLogger, s.metrics.emptyStreamResponses)
		if err != nil {
			level.Error(reqLogger).Log("err", err)
			level.Warn(s.logger).Log("msg", "Store failure", "group", st.GroupKey(), "replica", st.ReplicaKey())
			bumpCounter(st.GroupKey(), st.ReplicaKey(), failedStores)
			totalFailedStores++
			if r.PartialResponseStrategy == storepb.PartialResponseStrategy_GROUP_REPLICA {
				if checkGroupReplicaErrors(st, err) != nil {
					return err
				}
			} else if !r.PartialResponseDisabled || r.PartialResponseStrategy == storepb.PartialResponseStrategy_WARN {
				if err := srv.Send(storepb.NewWarnSeriesResponse(err)); err != nil {
					return err
				}
				continue
			} else {
				return err
			}
		}

		storeResponses = append(storeResponses, respSet)
		defer respSet.Close()
	}

	level.Debug(reqLogger).Log("msg", "Series: started fanout streams", "status", strings.Join(storeDebugMsgs, ";"))

	respHeap := NewResponseDeduplicator(NewProxyResponseLoserTree(storeResponses...))
	for respHeap.Next() {
		resp := respHeap.At()

		if resp.GetWarning() != "" {
			totalFailedStores++
			level.Error(s.logger).Log("msg", "Series: warning from store", "warning", resp.GetWarning())
			if r.PartialResponseStrategy == storepb.PartialResponseStrategy_GROUP_REPLICA {
				// TODO: attribute the warning to the store(group key and replica key) that produced it.
				// Each client streams a sequence of time series, so it's not trivial to attribute the warning to a specific client.
				if totalFailedStores > 1 {
					level.Error(reqLogger).Log("msg", "more than one stores have failed")
					// If we don't know which store has failed, we can tolerate at most one failed store.
					return status.Error(codes.Aborted, resp.GetWarning())
				}
			} else if r.PartialResponseDisabled || r.PartialResponseStrategy == storepb.PartialResponseStrategy_ABORT {
				return status.Error(codes.Aborted, resp.GetWarning())
			}
		}

		if err := srv.Send(resp); err != nil {
			return status.Error(codes.Unknown, errors.Wrap(err, "send series response").Error())
		}
	}

	return nil
}

// storeMatches returns boolean if the given store may hold data for the given label matchers, time ranges and debug store matches gathered from context.
func storeMatches(ctx context.Context, s Client, debugLogging bool, mint, maxt int64, matchers ...*labels.Matcher) (ok bool, reason string) {
	var storeDebugMatcher [][]*labels.Matcher
	if ctxVal := ctx.Value(StoreMatcherKey); ctxVal != nil {
		if value, ok := ctxVal.([][]*labels.Matcher); ok {
			storeDebugMatcher = value
		}
	}

	storeMinTime, storeMaxTime := s.TimeRange()
	if mint > storeMaxTime || maxt < storeMinTime {
		if debugLogging {
			reason = fmt.Sprintf("does not have data within this time period: [%v,%v]. Store time ranges: [%v,%v]", mint, maxt, storeMinTime, storeMaxTime)
		}
		return false, reason
	}

	if ok, reason := storeMatchDebugMetadata(s, storeDebugMatcher); !ok {
		return false, reason
	}

	extLset := s.LabelSets()
	if !labelSetsMatch(matchers, extLset...) {
		if debugLogging {
			reason = fmt.Sprintf("external labels %v does not match request label matchers: %v", extLset, matchers)
		}
		return false, reason
	}
	return true, ""
}

// storeMatchDebugMetadata return true if the store's address match the storeDebugMatchers.
func storeMatchDebugMetadata(s Client, storeDebugMatchers [][]*labels.Matcher) (ok bool, reason string) {
	if len(storeDebugMatchers) == 0 {
		return true, ""
	}

	addr, isLocal := s.Addr()
	if isLocal {
		return false, "the store is not remote, cannot match __address__"
	}

	match := false
	for _, sm := range storeDebugMatchers {
		match = match || labelSetsMatch(sm, labels.FromStrings("__address__", addr))
	}
	if !match {
		return false, fmt.Sprintf("__address__ %v does not match debug store metadata matchers: %v", addr, storeDebugMatchers)
	}
	return true, ""
}

// labelSetsMatch returns false if all label-set do not match the matchers (aka: OR is between all label-sets).
func labelSetsMatch(matchers []*labels.Matcher, lset ...labels.Labels) bool {
	if len(lset) == 0 {
		return true
	}

	for _, ls := range lset {
		notMatched := false
		for _, m := range matchers {
			if lv := ls.Get(m.Name); ls.Has(m.Name) && !m.Matches(lv) {
				notMatched = true
				break
			}
		}
		if !notMatched {
			return true
		}
	}
	return false
}

// LabelNames returns all known label names.
func (s *ProxyStore) LabelNames(ctx context.Context, r *storepb.LabelNamesRequest) (
	*storepb.LabelNamesResponse, error,
) {
	var (
		warnings       []string
		names          [][]string
		mtx            sync.Mutex
		g, gctx        = errgroup.WithContext(ctx)
		storeDebugMsgs []string
	)

	// We may arrive here either via the promql engine
	// or as a result of a grpc call in layered queries
	tenant, foundTenant := tenancy.GetTenantFromGRPCMetadata(gctx)
	if !foundTenant {
		level.Debug(s.logger).Log("msg", "using tenant from context instead of metadata")
		if gctx.Value(tenancy.TenantKey) != nil {
			tenant = gctx.Value(tenancy.TenantKey).(string)
		}
	}

	gctx = metadata.AppendToOutgoingContext(gctx, tenancy.DefaultTenantHeader, tenant)
	level.Debug(s.logger).Log("msg", "Tenant info in LabelNames()", "tenant", tenant)

	for _, st := range s.stores() {
		st := st

		// We might be able to skip the store if its meta information indicates it cannot have series matching our query.
		if ok, reason := storeMatches(gctx, st, s.debugLogging, r.Start, r.End); !ok {
			if s.debugLogging {
				storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s filtered out due to: %v", st, reason))
			}
			continue
		}
		matches, extraMatchers := s.tsdbSelector.MatchLabelSets(st.LabelSets()...)
		if !matches {
			if s.debugLogging {
				storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s filtered out due to: %v", st, "tsdb selector"))
			}
			continue
		}

		if s.debugLogging {
			storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s queried", st))
		}

		g.Go(func() error {
			resp, err := st.LabelNames(gctx, &storepb.LabelNamesRequest{
				PartialResponseDisabled: r.PartialResponseDisabled,
				Start:                   r.Start,
				End:                     r.End,
				Matchers:                append(r.Matchers, MatchersForLabelSets(extraMatchers)...),
				WithoutReplicaLabels:    r.WithoutReplicaLabels,
			})
			if err != nil {
				err = errors.Wrapf(err, "fetch label names from store %s", st)
				if r.PartialResponseDisabled {
					return err
				}

				mtx.Lock()
				warnings = append(warnings, err.Error())
				mtx.Unlock()
				return nil
			}

			mtx.Lock()
			warnings = append(warnings, resp.Warnings...)
			names = append(names, resp.Names)
			mtx.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	level.Debug(s.logger).Log("msg", strings.Join(storeDebugMsgs, ";"))
	return &storepb.LabelNamesResponse{
		Names:    strutil.MergeUnsortedSlices(names...),
		Warnings: warnings,
	}, nil
}

// LabelValues returns all known label values for a given label name.
func (s *ProxyStore) LabelValues(ctx context.Context, r *storepb.LabelValuesRequest) (
	*storepb.LabelValuesResponse, error,
) {
	var (
		warnings       []string
		all            [][]string
		mtx            sync.Mutex
		g, gctx        = errgroup.WithContext(ctx)
		storeDebugMsgs []string
	)
	if r.Label == "" {
		return nil, status.Error(codes.InvalidArgument, "label name parameter cannot be empty")
	}

	// We may arrive here either via the promql engine
	// or as a result of a grpc call in layered queries
	tenant, foundTenant := tenancy.GetTenantFromGRPCMetadata(gctx)
	if !foundTenant {
		level.Debug(s.logger).Log("msg", "using tenant from context instead of metadata")
		if gctx.Value(tenancy.TenantKey) != nil {
			tenant = gctx.Value(tenancy.TenantKey).(string)
		}
	}

	gctx = metadata.AppendToOutgoingContext(gctx, tenancy.DefaultTenantHeader, tenant)
	level.Debug(s.logger).Log("msg", "Tenant info in LabelValues()", "tenant", tenant)

	for _, st := range s.stores() {
		st := st

		storeAddr, isLocalStore := st.Addr()
		storeID := labelpb.PromLabelSetsToString(st.LabelSets())
		if storeID == "" {
			storeID = "Store Gateway"
		}

		// We might be able to skip the store if its meta information indicates it cannot have series matching our query.
		if ok, reason := storeMatches(gctx, st, s.debugLogging, r.Start, r.End); !ok {
			if s.debugLogging {
				storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s filtered out due to: %v", st, reason))
			}
			continue
		}
		matches, extraMatchers := s.tsdbSelector.MatchLabelSets(st.LabelSets()...)
		if !matches {
			if s.debugLogging {
				storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s filtered out due to: %v", st, "tsdb selector"))
			}
			continue
		}
		if s.debugLogging {
			storeDebugMsgs = append(storeDebugMsgs, fmt.Sprintf("Store %s queried", st))
		}

		g.Go(func() error {
			span, spanCtx := tracing.StartSpan(gctx, "proxy.label_values", tracing.Tags{
				"store.id":       storeID,
				"store.addr":     storeAddr,
				"store.is_local": isLocalStore,
			})
			defer span.Finish()

			resp, err := st.LabelValues(spanCtx, &storepb.LabelValuesRequest{
				Label:                   r.Label,
				PartialResponseDisabled: r.PartialResponseDisabled,
				Start:                   r.Start,
				End:                     r.End,
				Matchers:                append(r.Matchers, MatchersForLabelSets(extraMatchers)...),
				WithoutReplicaLabels:    r.WithoutReplicaLabels,
			})
			if err != nil {
				msg := "fetch label values from store %s"
				err = errors.Wrapf(err, msg, st)
				if r.PartialResponseDisabled {
					return err
				}

				mtx.Lock()
				warnings = append(warnings, errors.Wrapf(err, msg, st).Error())
				mtx.Unlock()
				return nil
			}

			mtx.Lock()
			warnings = append(warnings, resp.Warnings...)
			all = append(all, resp.Values)
			mtx.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	level.Debug(s.logger).Log("msg", strings.Join(storeDebugMsgs, ";"))
	return &storepb.LabelValuesResponse{
		Values:   strutil.MergeUnsortedSlices(all...),
		Warnings: warnings,
	}, nil
}
