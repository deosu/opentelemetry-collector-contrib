// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aerospikereceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/aerospikereceiver"

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/scrapertest"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/aerospikereceiver/internal/metadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/aerospikereceiver/mocks"
)

func TestNewAerospikeReceiver_BadEndpoint(t *testing.T) {
	testCases := []struct {
		name     string
		endpoint string
		errMsg   string
	}{
		{
			name:     "no port",
			endpoint: "localhost",
			errMsg:   "missing port in address",
		},
		{
			name:     "no address",
			endpoint: "",
			errMsg:   "missing port in address",
		},
	}

	cs, err := consumer.NewMetrics(func(ctx context.Context, ld pmetric.Metrics) error { return nil })
	require.NoError(t, err)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Endpoint: tc.endpoint}
			receiver, err := newAerospikeReceiver(component.ReceiverCreateSettings{}, cfg, cs)
			require.ErrorContains(t, err, tc.errMsg)
			require.Nil(t, receiver)
		})
	}
}

func TestScrape_CollectClusterMetrics(t *testing.T) {
	t.Parallel()

	logger, err := zap.NewDevelopment()
	require.NoError(t, err)
	now := pcommon.NewTimestampFromTime(time.Now().UTC())

	expectedMB := metadata.NewMetricsBuilder(metadata.DefaultMetricsSettings(), component.NewDefaultBuildInfo())

	require.NoError(t, expectedMB.RecordAerospikeNodeConnectionOpenDataPoint(now, "22", metadata.AttributeConnectionTypeClient))
	expectedMB.EmitForResource(metadata.WithAerospikeNodeName("BB990C28F270008"))

	require.NoError(t, expectedMB.RecordAerospikeNamespaceMemoryFreeDataPoint(now, "45"))
	expectedMB.EmitForResource(metadata.WithAerospikeNamespace("test"), metadata.WithAerospikeNodeName("BB990C28F270008"))

	require.NoError(t, expectedMB.RecordAerospikeNamespaceMemoryFreeDataPoint(now, "30"))
	expectedMB.EmitForResource(metadata.WithAerospikeNamespace("bar"), metadata.WithAerospikeNodeName("BB990C28F270008"))

	require.NoError(t, expectedMB.RecordAerospikeNodeConnectionOpenDataPoint(now, "1", metadata.AttributeConnectionTypeClient))
	expectedMB.EmitForResource(metadata.WithAerospikeNodeName("BB990C28F270009"))

	require.NoError(t, expectedMB.RecordAerospikeNamespaceMemoryUsageDataPoint(now, "128", metadata.AttributeNamespaceComponentData))
	expectedMB.EmitForResource(metadata.WithAerospikeNamespace("test"), metadata.WithAerospikeNodeName("BB990C28F270009"))

	// require.NoError(t, expectedMB.RecordAerospikeNamespaceMemoryUsageDataPoint(now, "badval", metadata.AttributeNamespaceComponentData))
	// expectedMB.EmitForResource(metadata.WithAerospikeNamespace("bar"), metadata.WithAerospikeNodeName("BB990C28F270009"))

	initialClient := mocks.NewAerospike(t)
	initialClient.On("Info").Return(clusterInfo{
		"BB990C28F270008": metricsMap{
			"node":               "BB990C28F270008",
			"client_connections": "22",
		},
		"BB990C28F270009": metricsMap{
			"node":               "BB990C28F270009",
			"client_connections": "1",
		},
	}, nil)

	initialClient.On("NamespaceInfo").Return(namespaceInfo{
		"BB990C28F270008": map[string]map[string]string{
			"test": metricsMap{
				"name":            "test",
				"memory_free_pct": "45",
			},
			"bar": metricsMap{
				"name":            "bar",
				"memory_free_pct": "30",
			},
		},
		"BB990C28F270009": map[string]map[string]string{
			"test": metricsMap{
				"name":                   "test",
				"memory_used_data_bytes": "128",
			},
			"bar": metricsMap{
				"name":                   "bar",
				"memory_used_data_bytes": "badval",
			},
		},
	}, nil)

	initialClient.On("Close").Return(nil)

	clientFactory := func(host string, port int) (Aerospike, error) {
		switch fmt.Sprintf("%s:%d", host, port) {
		case "localhost:3000":
			return initialClient, nil
		case "localhost:3002":
			return nil, errors.New("connection timeout")
		}

		return nil, errors.New("unexpected endpoint")
	}
	receiver := &aerospikeReceiver{
		host:          "localhost",
		port:          3000,
		clientFactory: clientFactory,
		mb:            metadata.NewMetricsBuilder(metadata.DefaultMetricsSettings(), component.NewDefaultBuildInfo()),
		logger:        logger.Sugar(),
		config: &Config{
			CollectClusterMetrics: true,
		},
	}

	require.NoError(t, receiver.start(context.Background(), componenttest.NewNopHost()))

	actualMetrics, err := receiver.scrape(context.Background())
	require.EqualError(t, err, "failed to parse int64 for AerospikeNamespaceMemoryUsage, value was badval: strconv.ParseInt: parsing \"badval\": invalid syntax")

	expectedMetrics := expectedMB.Emit()
	require.NoError(t, scrapertest.CompareMetrics(expectedMetrics, actualMetrics))

	require.NoError(t, receiver.shutdown(context.Background()))

	initialClient.AssertExpectations(t)

	receiverConnErr := &aerospikeReceiver{
		host:          "localhost",
		port:          3002,
		clientFactory: clientFactory,
		mb:            metadata.NewMetricsBuilder(metadata.DefaultMetricsSettings(), component.NewDefaultBuildInfo()),
		logger:        logger.Sugar(),
		config: &Config{
			CollectClusterMetrics: true,
		},
	}

	initialClient.AssertNumberOfCalls(t, "Close", 1)

	err = receiverConnErr.start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err)
	require.Equal(t, receiverConnErr.client, nil, "client should be set to nil because of connection error")

}