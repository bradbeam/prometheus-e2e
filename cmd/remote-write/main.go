package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/storage/remote"
)

func main() {
	port := flag.Int("port", 8080, "Port to listen on")
	flag.Parse()

	server := Server{remoteWriteMetrics: make(map[model.Fingerprint]model.Sample)}

	mux := http.NewServeMux()
	// TODO might be worth switching this out for remote.NewWriteHandler
	// but that may introduce more complexity than it's worth
	mux.Handle("/write", server.remoteWriteReceiver())
	mux.Handle("/metrics", server.metrics())

	log.Fatal(http.ListenAndServe(fmt.Sprintf("localhost:%d", *port), mux))
}

type Server struct {
	sync.Mutex
	remoteWriteMetrics map[model.Fingerprint]model.Sample
}

func (s *Server) remoteWriteReceiver() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.Lock()
		defer s.Unlock()

		req, err := remote.DecodeWriteRequest(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		for _, ts := range req.Timeseries {
			m := make(model.Metric, len(ts.Labels))
			for _, l := range ts.Labels {
				m[model.LabelName(l.Name)] = model.LabelValue(l.Value)
			}

			if len(ts.Samples) != 1 {
				log.Println("expected 1 sample, got", len(ts.Samples))
				continue
			}

			s.remoteWriteMetrics[m.Fingerprint()] = model.Sample{
				Metric:    m,
				Value:     model.SampleValue(ts.Samples[0].Value),
				Timestamp: model.Time(ts.Samples[0].Timestamp),
			}
		}
	}
}

func (s *Server) metrics() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		s.Lock()
		rwMetrics := s.remoteWriteMetrics
		s.Unlock()

		var sb strings.Builder
		for _, sample := range rwMetrics {
			sb.WriteString(sample.Metric.String())
			sb.WriteString(" ")
			sb.WriteString(fmt.Sprintf("%f\n", sample.Value))
		}

		var p expfmt.TextParser
		metricFamilies, err := p.TextToMetricFamilies(strings.NewReader(sb.String()))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "error parsing metrics: %s", err)
			return
		}

		enc := expfmt.NewEncoder(w, expfmt.FmtText)
		for _, mf := range metricFamilies {
			if err := enc.Encode(mf); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "error encoding metrics: %s", err)
				return
			}
		}
	}
}
