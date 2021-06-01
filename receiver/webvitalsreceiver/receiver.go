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
	}
	return r, nil
}

func (r *webVitalsReceiver) Start(_ context.Context, host component.Host) error {
	if host == nil {
		return errors.New("nil host")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	err := componenterror.ErrNilNextConsumer
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
	ctx = obsreport.ReceiverContext(ctx, r.config.ID(), transportType)

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

	if consumerErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(errNextConsumerRespBody)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func (r *webVitalsReceiver) webVitalsToMetric(blob []byte) (metrics pdata.Metrics, err error) {
	var payload webVitalPayload
	t := time.Now()
	payload, _ = r.deserializeFromJSON(blob)

	ms := pdata.NewMetrics()
	rm := ms.ResourceMetrics().AppendEmpty()
	ilms := rm.InstrumentationLibraryMetrics()
	ilmsMetric := ilms.AppendEmpty()
	metric := ilmsMetric.Metrics().AppendEmpty()
	metric.SetName(payload.Name)
	metric.SetUnit("ms")
	metric.SetDataType(pdata.MetricDataTypeDoubleGauge)
	dg := metric.DoubleGauge()
	dp := dg.DataPoints().AppendEmpty()
	dp.SetValue(payload.Value)
	dp.LabelsMap().Insert("id", payload.Id)
	dp.SetTimestamp(pdata.TimestampFromTime(t))
	dp.SetStartTimestamp(pdata.TimestampFromTime(t))
	return ms, nil
}

func (r *webVitalsReceiver) deserializeFromJSON(jsonBlob []byte) (t webVitalPayload, err error) {
	if err = json.Unmarshal(jsonBlob, &t); err != nil {
		return webVitalPayload{}, err
	}
	return t, nil
}

func (r *webVitalsReceiver) Shutdown(context.Context) error {
	err := componenterror.ErrNilNextConsumer
	r.stopOnce.Do(func() {
		err = r.server.Close()
		r.shutdownWG.Wait()
	})
	return err
}
