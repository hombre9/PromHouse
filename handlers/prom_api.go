// PromHouse
// Copyright (C) 2017 Percona LLC
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package handlers

import (
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/sirupsen/logrus"

	"github.com/Percona-Lab/PromHouse/storages"
)

type PromAPI struct {
	Storage storages.Storage
	Logger  *logrus.Entry
}

func readPB(req *http.Request, pb proto.Message) error {
	compressed, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return err
	}
	b, err := snappy.Decode(nil, compressed)
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, pb)
}

func (p *PromAPI) Read(rw http.ResponseWriter, req *http.Request) error {
	var request prompb.ReadRequest
	if err := readPB(req, &request); err != nil {
		return err
	}

	// convert to query
	queries := make([]storages.Query, len(request.Queries))
	for i, rq := range request.Queries {
		empty := true
		q := storages.Query{
			Start:    model.Time(rq.StartTimestampMs),
			End:      model.Time(rq.EndTimestampMs),
			Matchers: make([]storages.Matcher, len(rq.Matchers)),
		}
		for j, m := range rq.Matchers {
			var t storages.MatchType
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				t = storages.MatchEqual
			case prompb.LabelMatcher_NEQ:
				t = storages.MatchNotEqual
			case prompb.LabelMatcher_RE:
				t = storages.MatchRegexp
			case prompb.LabelMatcher_NRE:
				t = storages.MatchNotRegexp
			default:
				return fmt.Errorf("unexpected matcher %d", m.Type)
			}

			q.Matchers[j] = storages.Matcher{
				Type:  t,
				Name:  model.LabelName(m.Name),
				Value: m.Value,
			}
			if m.Value != "" {
				empty = false
			}
		}

		if empty {
			p.Logger.Panicf("expectation failed: at least one matcher should have non-empty label value")
		}
		queries[i] = q
	}

	// read from storage
	p.Logger.Infof("Queries: %s", queries)
	data, err := p.Storage.Read(req.Context(), queries)
	if err != nil {
		return err
	}
	p.Logger.Debugf("Response data:\n%s", data)

	// convert to response
	response := prompb.ReadResponse{
		Results: make([]*prompb.QueryResult, len(data)),
	}
	var series, samples int
	for i, m := range data {
		qr := &prompb.QueryResult{
			Timeseries: make([]*prompb.TimeSeries, len(m)),
		}
		for j, ss := range m {
			ts := &prompb.TimeSeries{
				Labels:  make([]*prompb.Label, 0, len(ss.Metric)),
				Samples: make([]*prompb.Sample, len(ss.Values)),
			}
			for n, v := range ss.Metric {
				ts.Labels = append(ts.Labels, &prompb.Label{
					Name:  string(n),
					Value: string(v),
				})
			}
			for k, sp := range ss.Values {
				ts.Samples[k] = &prompb.Sample{
					Timestamp: int64(sp.Timestamp),
					Value:     float64(sp.Value),
				}
				samples++
			}
			qr.Timeseries[j] = ts
			series++
		}
		response.Results[i] = qr
	}
	p.Logger.Infof("Response: %d matrixes, %d time series, %d samples.", len(data), series, samples)

	// marshal, encode and write response
	b, err := proto.Marshal(&response)
	if err != nil {
		return err
	}
	rw.Header().Set("Content-Type", "application/x-protobuf")
	rw.Header().Set("Content-Encoding", "snappy")
	compressed := snappy.Encode(nil, b)
	_, err = rw.Write(compressed)
	return err
}

func (p *PromAPI) Write(rw http.ResponseWriter, req *http.Request) error {
	var request prompb.WriteRequest
	if err := readPB(req, &request); err != nil {
		return err
	}

	// convert to matrix
	var samples int
	data := make(model.Matrix, len(request.Timeseries))
	for i, ts := range request.Timeseries {
		ss := &model.SampleStream{
			Metric: make(model.Metric, len(ts.Labels)),
			Values: make([]model.SamplePair, len(ts.Samples)),
		}
		for _, l := range ts.Labels {
			n := model.LabelName(l.Name)
			v := model.LabelValue(l.Value)
			if !n.IsValid() {
				return fmt.Errorf("invalid label name %q", n)
			}
			if n == model.MetricNameLabel {
				if !model.IsValidMetricName(v) {
					return fmt.Errorf("invalid metric name %q", v)
				}
			} else if !v.IsValid() {
				return fmt.Errorf("invalid value %q for label %s", v, n)
			}
			ss.Metric[n] = v
		}
		for j, s := range ts.Samples {
			ss.Values[j] = model.SamplePair{
				Timestamp: model.Time(s.Timestamp),
				Value:     model.SampleValue(s.Value),
			}
			samples++
		}
		data[i] = ss
	}

	// write to storage
	p.Logger.Infof("Writing %d time series, %d samples.", len(data), samples)
	p.Logger.Debugf("Writing data:\n%s", data)
	return p.Storage.Write(req.Context(), data)
}
