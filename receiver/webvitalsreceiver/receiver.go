// Copyright  OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webvitalsreceiver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenterror"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/obsreport"
	"go.uber.org/zap"
)

type webVitalsReceiver struct {
	mu     sync.Mutex
	logger *zap.Logger
	config *Config

	host         component.Host
	nextConsumer consumer.Metrics
	instanceName string
	server       *http.Server

	startOnce  sync.Once
	stopOnce   sync.Once
	shutdownWG sync.WaitGroup
}

type webVitalPayload struct {
	Id        string  `json:"id"`
	Name      string  `json:"name"`
	StartTime float64 `json:"startTime"`
	Value     float64 `json:"value"`
}

var errNextConsumerRespBody = []byte(`"Internal Server Error"`)

var _ http.Handler = (*webVitalsReceiver)(nil)

func New(
	logger *zap.Logger,
	config Config,
	nextConsumer consumer.Metrics,
) (*webVitalsReceiver, error) {
	if nextConsumer == nil {
		return nil, componenterror.ErrNilNextConsumer
	}

	if config.HTTPServerSettings.Endpoint == "" {
		config.HTTPServerSettings.Endpoint = defaultBindEndpoint // TODO: check this
	}

	r := &webVitalsReceiver{
		logger:       logger,
		config:       &config,
		nextConsumer: nextConsumer,
		instanceName: config.Name(),
	}
	return r, nil
}

func (r *webVitalsReceiver) Start(_ context.Context, host component.Host) error {
	if host == nil {
		return errors.New("nil host")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	err := componenterror.ErrAlreadyStarted
	r.startOnce.Do(func() {
		err = nil
		r.host = host
		r.server = r.config.HTTPServerSettings.ToServer(r)
		listener, err := r.config.HTTPServerSettings.ToListener()
		if err != nil {
			return
		}
		r.shutdownWG.Add(1)
		go func() {
			defer r.shutdownWG.Done()
			if errHTTP := r.server.Serve(listener); errHTTP != http.ErrServerClosed {
				host.ReportFatalError(errHTTP)
			}
		}()
	})

	return err
}

func (r *webVitalsReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if c, ok := client.FromHTTP(req); ok {
		ctx = client.NewContext(ctx, c)
	}

	transportType := req.Header.Get("Content-Type")
	ctx = obsreport.ReceiverContext(ctx, r.instanceName, transportType)
	ctx = obsreport.StartMetricsReceiveOp(ctx, r.instanceName, transportType)

	slurp, _ := ioutil.ReadAll(req.Body)
	if c, ok := req.Body.(io.Closer); ok {
		_ = c.Close()
	}
	_ = req.Body.Close()

	var md pdata.Metrics
	var err error
	md, err = r.webVitalsToMetric(slurp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	consumerErr := r.nextConsumer.ConsumeMetrics(ctx, md)
	obsreport.EndMetricsReceiveOp(ctx, "segment", 1, consumerErr)

	if consumerErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(errNextConsumerRespBody)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func (r *webVitalsReceiver) webVitalsToMetric(blob []byte) (metrics pdata.Metrics, err error) {
	var payload webVitalPayload
	payload, err = r.deserializeFromJSON(blob)

	datapoint := pdata.NewIntDataPoint()
	datapoint.SetValue(int64(payload.Value))
	datapoint.SetTimestamp(pdata.TimestampFromTime(r.convertFloatTimeToTime(payload.StartTime)))
	datapoint.LabelsMap().Insert("id", payload.Id)

	metric := pdata.NewMetric()
	metric.SetName(payload.Name)
	metric.SetUnit("count")
	metric.SetDataType(pdata.MetricDataTypeIntGauge)
	metric.IntGauge().DataPoints().Append(datapoint)

	ilmetrics := pdata.NewInstrumentationLibraryMetrics()
	ilmetrics.Metrics().Append(metric)

	metricSeries := pdata.NewMetrics()
	metricSeries.ResourceMetrics().Resize(1)
	metricSeries.ResourceMetrics().At(0).InstrumentationLibraryMetrics().Resize(0)
	metricSeries.ResourceMetrics().At(0).InstrumentationLibraryMetrics().Append(ilmetrics)

	return metricSeries, nil
}

func (r *webVitalsReceiver) deserializeFromJSON(jsonBlob []byte) (t webVitalPayload, err error) {
	if err = json.Unmarshal(jsonBlob, &t); err != nil {
		return webVitalPayload{}, err
	}
	return t, nil
}

func (r *webVitalsReceiver) Shutdown(context.Context) error {
	err := componenterror.ErrAlreadyStopped
	r.stopOnce.Do(func() {
		err = r.server.Close()
		r.shutdownWG.Wait()
	})
	return err
}

func (r *webVitalsReceiver) convertFloatTimeToTime(t float64) (ct time.Time) {
	sec, dec := math.Modf(t)
	return time.Unix(int64(sec), int64(dec*(1e9)))
}
