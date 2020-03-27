// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package harness

import (
	"bytes"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/beam/sdks/go/pkg/beam/core/graph/coder"
	"github.com/apache/beam/sdks/go/pkg/beam/core/graph/mtime"
	"github.com/apache/beam/sdks/go/pkg/beam/core/metrics"
	"github.com/apache/beam/sdks/go/pkg/beam/core/runtime/exec"
	fnpb "github.com/apache/beam/sdks/go/pkg/beam/model/fnexecution_v1"
	ppb "github.com/apache/beam/sdks/go/pkg/beam/model/pipeline_v1"
	"github.com/golang/protobuf/ptypes"
)

type mUrn uint32
type mType uint32

// TODO: Pull these from the protos.
var sUrns = []string{
	"beam:metric:user:sum_int64:v1",
	"beam:metric:user:sum_double:v1",
	"beam:metric:user:distribution_int64:v1",
	"beam:metric:user:distribution_double:v1",
	"beam:metric:user:latest_int64:v1",
	"beam:metric:user:latest_double:v1",
	"beam:metric:user:top_n_int64:v1",
	"beam:metric:user:top_n_double:v1",
	"beam:metric:user:bottom_n_int64:v1",
	"beam:metric:user:bottom_n_double:v1",

	"beam:metric:element_count:v1",
	"beam:metric:sampled_byte_size:v1",

	"beam:metric:pardo_execution_time:start_bundle_msecs:v1",
	"beam:metric:pardo_execution_time:process_bundle_msecs:v1",
	"beam:metric:pardo_execution_time:finish_bundle_msecs:v1",
	"beam:metric:ptransform_execution_time:total_msecs:v1",

	"beam:metric:ptransform_progress:remaining:v1",
	"beam:metric:ptransform_progress:completed:v1",

	"TestingSentinelUrn", // Must remain last.
}

const (
	urnUserSumInt64 mUrn = iota
	urnUserSumFloat64
	urnUserDistInt64
	urnUserDistFloat64
	urnUserLatestMsInt64
	urnUserLatestMsFloat64
	urnUserTopNInt64
	urnUserTopNFloat64
	urnUserBottomNInt64
	urnUserBottomNFloat64

	urnElementCount
	urnSampledByteSize

	urnStartBundle
	urnProcessBundle
	urnFinishBundle
	urnTransformTotalTime

	urnProgressRemaining
	urnProgressCompleted

	urnTestSentinel // Must remain last.
)

var sTypes = []string{
	"beam:metrics:sum_int64:v1",
	"beam:metrics:sum_double:v1",
	"beam:metrics:distribution_int64:v1",
	"beam:metrics:distribution_double:v1",
	"beam:metrics:latest_int64:v1",
	"beam:metrics:latest_double:v1",
	"beam:metrics:top_n_int64:v1",
	"beam:metrics:top_n_double:v1",
	"beam:metrics:bottom_n_int64:v1",
	"beam:metrics:bottom_n_double:v1",
	"beam:metrics:monitoring_table:v1",
	"beam:metrics:progress:v1",

	"TestingSentinelType", // Must remain last.
}

const (
	typeSumInt64 mType = iota
	typeSumFloat64
	typeDistInt64
	typeDistFloat64
	typeLatestMsInt64
	typeLatestMsFloat64
	typeTopNInt64
	typeTopNFloat64
	typeBottomNInt64
	typeBottomNFloat64

	typeMonitoringTable
	typeProgress

	typeTestSentinel // Must remain last.
)

// urnToType maps the urn to it's encoding type.
// This function is written to be inlinable by the compiler.
func urnToType(u mUrn) mType {
	switch u {
	case urnUserSumInt64, urnElementCount, urnStartBundle, urnProcessBundle, urnFinishBundle, urnTransformTotalTime:
		return typeSumInt64
	case urnUserSumFloat64:
		return typeSumFloat64
	case urnUserDistInt64, urnSampledByteSize:
		return typeDistInt64
	case urnUserDistFloat64:
		return typeDistFloat64
	case urnUserLatestMsInt64:
		return typeLatestMsInt64
	case urnUserLatestMsFloat64:
		return typeLatestMsFloat64
	case urnUserTopNInt64:
		return typeTopNInt64
	case urnUserTopNFloat64:
		return typeTopNFloat64
	case urnUserBottomNInt64:
		return typeSumInt64
	case urnUserBottomNFloat64:
		return typeBottomNFloat64

	case urnProgressRemaining, urnProgressCompleted:
		return typeProgress

	case urnTestSentinel:
		return typeTestSentinel

	default:
		panic("metric urn without specified type" + sUrns[u])
	}
}

type shortKey struct {
	metrics.Labels
	Urn mUrn // Urns fully specify their type.
}

// shortIDCache retains lookup caches for short ids to the full monitoring
// info metadata.
//
// TODO: 2020/03/26 - measure mutex overhead vs sync.Map for this case.
// sync.Map might have lower contention for this read heavy load.
type shortIDCache struct {
	mu              sync.Mutex
	labels2ShortIds map[shortKey]string
	shortIds2Infos  map[string]*ppb.MonitoringInfo

	lastShortID int64
}

func newShortIDCache() *shortIDCache {
	return &shortIDCache{
		labels2ShortIds: make(map[shortKey]string),
		shortIds2Infos:  make(map[string]*ppb.MonitoringInfo),
	}
}

func (c *shortIDCache) getNextShortID() string {
	id := atomic.AddInt64(&c.lastShortID, 1)
	// No reason not to use the smallest string short ids possible.
	return strconv.FormatInt(id, 36)
}

// getShortID returns the short id for the given metric, and if
// it doesn't exist yet, stores the metadata.
// Assumes shortMu lock is held.
func (c *shortIDCache) getShortID(l metrics.Labels, urn mUrn) string {
	k := shortKey{l, urn}
	s, ok := c.labels2ShortIds[k]
	if ok {
		return s
	}
	s = c.getNextShortID()
	c.labels2ShortIds[k] = s
	c.shortIds2Infos[s] = &ppb.MonitoringInfo{
		Urn:    sUrns[urn],
		Type:   sTypes[urnToType(urn)],
		Labels: userLabels(l),
	}
	return s
}

func (c *shortIDCache) shortIdsToInfos(shortids []string) map[string]*ppb.MonitoringInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := make(map[string]*ppb.MonitoringInfo, len(shortids))
	for _, s := range shortids {
		m[s] = c.shortIds2Infos[s]
	}
	return m
}

// Convenience package functions for production.
var defaultShortIDCache *shortIDCache

func init() {
	defaultShortIDCache = newShortIDCache()
}

func getShortID(l metrics.Labels, urn mUrn) string {
	return defaultShortIDCache.getShortID(l, urn)
}

func shortIdsToInfos(shortids []string) map[string]*ppb.MonitoringInfo {
	return defaultShortIDCache.shortIdsToInfos(shortids)
}

func monitoring(p *exec.Plan) (*fnpb.Metrics, []*ppb.MonitoringInfo, map[string][]byte) {
	store := p.Store()
	if store == nil {
		return nil, nil, nil
	}

	// Get the legacy style metrics.
	transforms := make(map[string]*fnpb.Metrics_PTransform)
	metrics.Extractor{
		SumInt64: func(l metrics.Labels, v int64) {
			pb := getTransform(transforms, l)
			pb.User = append(pb.User, &fnpb.Metrics_User{
				MetricName: toName(l),
				Data: &fnpb.Metrics_User_CounterData_{
					CounterData: &fnpb.Metrics_User_CounterData{
						Value: v,
					},
				},
			})
		},
		DistributionInt64: func(l metrics.Labels, count, sum, min, max int64) {
			pb := getTransform(transforms, l)
			pb.User = append(pb.User, &fnpb.Metrics_User{
				MetricName: toName(l),
				Data: &fnpb.Metrics_User_DistributionData_{
					DistributionData: &fnpb.Metrics_User_DistributionData{
						Count: count,
						Sum:   sum,
						Min:   min,
						Max:   max,
					},
				},
			})
		},
		GaugeInt64: func(l metrics.Labels, v int64, t time.Time) {
			ts, err := ptypes.TimestampProto(t)
			if err != nil {
				panic(err)
			}
			pb := getTransform(transforms, l)
			pb.User = append(pb.User, &fnpb.Metrics_User{
				MetricName: toName(l),
				Data: &fnpb.Metrics_User_GaugeData_{
					GaugeData: &fnpb.Metrics_User_GaugeData{
						Value:     v,
						Timestamp: ts,
					},
				},
			})
		},
	}.ExtractFrom(store)

	defaultShortIDCache.mu.Lock()
	defer defaultShortIDCache.mu.Unlock()

	// Get the MonitoringInfo versions.
	var monitoringInfo []*ppb.MonitoringInfo
	payloads := make(map[string][]byte)
	metrics.Extractor{
		SumInt64: func(l metrics.Labels, v int64) {
			payload, err := int64Counter(v)
			if err != nil {
				panic(err)
			}
			payloads[getShortID(l, urnUserSumInt64)] = payload

			monitoringInfo = append(monitoringInfo,
				&ppb.MonitoringInfo{
					Urn:     sUrns[urnUserSumInt64],
					Type:    sTypes[typeSumInt64],
					Labels:  userLabels(l),
					Payload: payload,
				})
		},
		DistributionInt64: func(l metrics.Labels, count, sum, min, max int64) {
			payload, err := int64Distribution(count, sum, min, max)
			if err != nil {
				panic(err)
			}
			payloads[getShortID(l, urnUserDistInt64)] = payload

			monitoringInfo = append(monitoringInfo,
				&ppb.MonitoringInfo{
					Urn:     sUrns[urnUserDistInt64],
					Type:    sTypes[typeDistInt64],
					Labels:  userLabels(l),
					Payload: payload,
				})
		},
		GaugeInt64: func(l metrics.Labels, v int64, t time.Time) {
			payload, err := int64Latest(t, v)
			if err != nil {
				panic(err)
			}
			payloads[getShortID(l, urnUserLatestMsInt64)] = payload

			monitoringInfo = append(monitoringInfo,
				&ppb.MonitoringInfo{
					Urn:     sUrns[urnUserLatestMsInt64],
					Type:    sTypes[typeLatestMsInt64],
					Labels:  userLabels(l),
					Payload: payload,
				})

		},
	}.ExtractFrom(store)

	// Get the execution monitoring information from the bundle plan.
	if snapshot, ok := p.Progress(); ok {
		// Legacy version.
		transforms[snapshot.ID] = &fnpb.Metrics_PTransform{
			ProcessedElements: &fnpb.Metrics_PTransform_ProcessedElements{
				Measured: &fnpb.Metrics_PTransform_Measured{
					OutputElementCounts: map[string]int64{
						snapshot.Name: snapshot.Count,
					},
				},
			},
		}
		// Monitoring info version.
		payload, err := int64Counter(snapshot.Count)
		if err == nil {
			monitoringInfo = append(monitoringInfo,
				&ppb.MonitoringInfo{
					Urn:  sUrns[urnElementCount],
					Type: sTypes[typeSumInt64],
					Labels: map[string]string{
						"PCOLLECTION": snapshot.PID,
					},
					Payload: payload,
				})
		}
	}

	return &fnpb.Metrics{
			Ptransforms: transforms,
		}, monitoringInfo,
		payloads
}

func userLabels(l metrics.Labels) map[string]string {
	return map[string]string{
		"PTRANSFORM": l.Transform(),
		"NAMESPACE":  l.Namespace(),
		"NAME":       l.Name(),
	}
}

func int64Counter(v int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := coder.EncodeVarInt(v, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func int64Latest(t time.Time, v int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := coder.EncodeVarInt(mtime.FromTime(t).Milliseconds(), &buf); err != nil {
		return nil, err
	}
	if err := coder.EncodeVarInt(v, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func int64Distribution(count, sum, min, max int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := coder.EncodeVarInt(count, &buf); err != nil {
		return nil, err
	}
	if err := coder.EncodeVarInt(sum, &buf); err != nil {
		return nil, err
	}
	if err := coder.EncodeVarInt(min, &buf); err != nil {
		return nil, err
	}
	if err := coder.EncodeVarInt(max, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func getTransform(transforms map[string]*fnpb.Metrics_PTransform, l metrics.Labels) *fnpb.Metrics_PTransform {
	if pb, ok := transforms[l.Transform()]; ok {
		return pb
	}
	pb := &fnpb.Metrics_PTransform{}
	transforms[l.Transform()] = pb
	return pb
}

func toName(l metrics.Labels) *fnpb.Metrics_User_MetricName {
	return &fnpb.Metrics_User_MetricName{
		Name:      l.Name(),
		Namespace: l.Namespace(),
	}
}
