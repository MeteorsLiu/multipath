package metrics

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	TUNPacketsTotal           = "multipath_tun_packets_total"
	TUNBytesTotal             = "multipath_tun_bytes_total"
	TUNErrorsTotal            = "multipath_tun_errors_total"
	TransportPacketsTotal     = "multipath_transport_packets_total"
	TransportBytesTotal       = "multipath_transport_bytes_total"
	TransportErrorsTotal      = "multipath_transport_errors_total"
	ProtocolFramesTotal       = "multipath_protocol_frames_total"
	ProtocolDecodeErrorsTotal = "multipath_protocol_decode_errors_total"
	SchedulePickTotal         = "multipath_schedule_pick_total"
	ScheduleNoRunnableTotal   = "multipath_schedule_no_runnable_total"
	ScheduleSkipTotal         = "multipath_schedule_skip_total"
	LaneEventsTotal           = "multipath_lane_events_total"
	LaneRTTMs                 = "multipath_lane_rtt_ms"
	LaneBandwidthBps          = "multipath_lane_bandwidth_bps"
	LaneProbeLossRatio        = "multipath_lane_probe_loss_ratio"
	BandwidthProbeEventsTotal = "multipath_bandwidth_probe_events_total"
	FECEventsTotal            = "multipath_fec_events_total"
	FECFlushTotal             = "multipath_fec_flush_total"
	QoSEventsTotal            = "multipath_qos_events_total"
	QoSDeliveredBps           = "multipath_qos_delivered_bps"
	LinkStatusEventsTotal     = "multipath_link_status_events_total"
	ProbeEventsTotal          = "multipath_probe_events_total"
	RuntimeInfo               = "multipath_runtime_info"
)

var Default = NewRegistry()

type Label struct {
	Name  string
	Value string
}

type Registry struct {
	gatherer prometheus.Gatherer
	counters map[string]*prometheus.CounterVec
	gauges   map[string]*prometheus.GaugeVec
}

type metricKind uint8

const (
	metricCounter metricKind = iota + 1
	metricGauge
)

type metricSpec struct {
	kind   metricKind
	help   string
	labels []string
}

var specs = map[string]metricSpec{
	TUNPacketsTotal: {
		kind:   metricCounter,
		help:   "Total TUN packets read from or written to the TUN device.",
		labels: []string{"direction"},
	},
	TUNBytesTotal: {
		kind:   metricCounter,
		help:   "Total TUN bytes read from or written to the TUN device.",
		labels: []string{"direction"},
	},
	TUNErrorsTotal: {
		kind:   metricCounter,
		help:   "Total TUN read or write errors.",
		labels: []string{"operation"},
	},
	TransportPacketsTotal: {
		kind:   metricCounter,
		help:   "Total transport packets read or written by carrier type.",
		labels: []string{"transport", "direction", "endpoint"},
	},
	TransportBytesTotal: {
		kind:   metricCounter,
		help:   "Total transport bytes read or written by carrier type.",
		labels: []string{"transport", "direction", "endpoint"},
	},
	TransportErrorsTotal: {
		kind:   metricCounter,
		help:   "Total transport operation errors by carrier type and operation.",
		labels: []string{"transport", "operation"},
	},
	ProtocolFramesTotal: {
		kind:   metricCounter,
		help:   "Total protocol frames encoded for transmit or decoded on receive.",
		labels: []string{"direction", "type", "session", "lane", "leg"},
	},
	ProtocolDecodeErrorsTotal: {
		kind:   metricCounter,
		help:   "Total protocol decode errors.",
		labels: []string{"leg"},
	},
	SchedulePickTotal: {
		kind:   metricCounter,
		help:   "Total schedule strategy lane picks.",
		labels: []string{"session", "lane", "frame_type", "leg"},
	},
	ScheduleNoRunnableTotal: {
		kind:   metricCounter,
		help:   "Total schedule attempts with no runnable lane.",
		labels: []string{"session", "frame_type"},
	},
	ScheduleSkipTotal: {
		kind:   metricCounter,
		help:   "Total schedule skips by reason.",
		labels: []string{"session", "lane", "reason"},
	},
	LaneEventsTotal: {
		kind:   metricCounter,
		help:   "Total lane lifecycle and fallback events.",
		labels: []string{"event", "session", "lane", "leg"},
	},
	LaneRTTMs: {
		kind:   metricGauge,
		help:   "Smoothed RTT in milliseconds by lane and transport leg.",
		labels: []string{"session", "lane", "leg"},
	},
	LaneBandwidthBps: {
		kind:   metricGauge,
		help:   "Estimated bandwidth in bits per second by lane and transport leg.",
		labels: []string{"session", "lane", "leg"},
	},
	LaneProbeLossRatio: {
		kind:   metricGauge,
		help:   "Last bandwidth probe loss ratio by lane and transport leg.",
		labels: []string{"session", "lane", "leg"},
	},
	BandwidthProbeEventsTotal: {
		kind:   metricCounter,
		help:   "Total bandwidth probe events.",
		labels: []string{"event", "session", "lane", "leg"},
	},
	FECEventsTotal: {
		kind:   metricCounter,
		help:   "Total FEC encode, repair, and recovery events.",
		labels: []string{"event", "session", "source_span"},
	},
	FECFlushTotal: {
		kind:   metricCounter,
		help:   "Total FEC flush-triggered repair emissions.",
		labels: []string{"session", "source_span"},
	},
	QoSEventsTotal: {
		kind:   metricCounter,
		help:   "Total receive-side QoS estimator events.",
		labels: []string{"event", "session", "lane", "leg", "reason"},
	},
	QoSDeliveredBps: {
		kind:   metricGauge,
		help:   "Receive-side QoS delivered bits per second by lane and transport leg.",
		labels: []string{"session", "lane", "leg"},
	},
	LinkStatusEventsTotal: {
		kind:   metricCounter,
		help:   "Total LINK_STATUS send/apply/drop events.",
		labels: []string{"event", "session", "lane", "leg", "reason"},
	},
	ProbeEventsTotal: {
		kind:   metricCounter,
		help:   "Total probe loop events.",
		labels: []string{"event", "direction"},
	},
	RuntimeInfo: {
		kind:   metricGauge,
		help:   "Runtime metadata exposed as a constant gauge.",
		labels: []string{"role", "fec", "paths"},
	},
}

func NewRegistry() *Registry {
	promRegistry := prometheus.NewRegistry()
	registry := &Registry{
		gatherer: promRegistry,
		counters: make(map[string]*prometheus.CounterVec),
		gauges:   make(map[string]*prometheus.GaugeVec),
	}

	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := specs[name]
		switch spec.kind {
		case metricCounter:
			vec := prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: name,
				Help: spec.help,
			}, spec.labels)
			promRegistry.MustRegister(vec)
			registry.counters[name] = vec
		case metricGauge:
			vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: name,
				Help: spec.help,
			}, spec.labels)
			promRegistry.MustRegister(vec)
			registry.gauges[name] = vec
		}
	}
	return registry
}

func L(name string, value any) Label {
	switch v := value.(type) {
	case string:
		return Label{Name: name, Value: v}
	case bool:
		return Label{Name: name, Value: strconv.FormatBool(v)}
	case int:
		return Label{Name: name, Value: strconv.Itoa(v)}
	case int8:
		return Label{Name: name, Value: strconv.FormatInt(int64(v), 10)}
	case int16:
		return Label{Name: name, Value: strconv.FormatInt(int64(v), 10)}
	case int32:
		return Label{Name: name, Value: strconv.FormatInt(int64(v), 10)}
	case int64:
		return Label{Name: name, Value: strconv.FormatInt(v, 10)}
	case uint:
		return Label{Name: name, Value: strconv.FormatUint(uint64(v), 10)}
	case uint8:
		return Label{Name: name, Value: strconv.FormatUint(uint64(v), 10)}
	case uint16:
		return Label{Name: name, Value: strconv.FormatUint(uint64(v), 10)}
	case uint32:
		return Label{Name: name, Value: strconv.FormatUint(uint64(v), 10)}
	case uint64:
		return Label{Name: name, Value: strconv.FormatUint(v, 10)}
	default:
		return Label{Name: name, Value: fmt.Sprint(value)}
	}
}

// LStr / LU64 / LU8 are typed alternatives to L that avoid boxing the value
// into `any`, removing one allocation per label on hot paths.
func LStr(name, value string) Label {
	return Label{Name: name, Value: value}
}

func LU64(name string, value uint64) Label {
	return Label{Name: name, Value: strconv.FormatUint(value, 10)}
}

func LU8(name string, value uint8) Label {
	return Label{Name: name, Value: strconv.FormatUint(uint64(value), 10)}
}

func AddCounter(name string, delta uint64, labels ...Label) {
	Default.AddCounter(name, delta, labels...)
}

func IncCounter(name string, labels ...Label) {
	Default.AddCounter(name, 1, labels...)
}

func SetGauge(name string, value float64, labels ...Label) {
	Default.SetGauge(name, value, labels...)
}

func (r *Registry) AddCounter(name string, delta uint64, labels ...Label) {
	if r == nil || delta == 0 {
		return
	}
	counter := r.counters[name]
	if counter == nil {
		return
	}
	metric, err := counterMetric(counter, specs[name], labels)
	if err != nil {
		return
	}
	metric.Add(float64(delta))
}

func (r *Registry) SetGauge(name string, value float64, labels ...Label) {
	if r == nil {
		return
	}
	gauge := r.gauges[name]
	if gauge == nil {
		return
	}
	metric, err := gaugeMetric(gauge, specs[name], labels)
	if err != nil {
		return
	}
	metric.Set(value)
}

func (r *Registry) Gatherer() prometheus.Gatherer {
	if r == nil || r.gatherer == nil {
		return prometheus.NewRegistry()
	}
	return r.gatherer
}

func counterMetric(counter *prometheus.CounterVec, spec metricSpec, labels []Label) (prometheus.Counter, error) {
	switch len(spec.labels) {
	case 0:
		return counter.GetMetricWithLabelValues()
	case 1:
		return counter.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]))
	case 2:
		return counter.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]))
	case 3:
		return counter.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]), labelValue(labels, spec.labels[2]))
	case 4:
		return counter.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]), labelValue(labels, spec.labels[2]), labelValue(labels, spec.labels[3]))
	case 5:
		return counter.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]), labelValue(labels, spec.labels[2]), labelValue(labels, spec.labels[3]), labelValue(labels, spec.labels[4]))
	default:
		values := make([]string, len(spec.labels))
		for i, name := range spec.labels {
			values[i] = labelValue(labels, name)
		}
		return counter.GetMetricWithLabelValues(values...)
	}
}

func gaugeMetric(gauge *prometheus.GaugeVec, spec metricSpec, labels []Label) (prometheus.Gauge, error) {
	switch len(spec.labels) {
	case 0:
		return gauge.GetMetricWithLabelValues()
	case 1:
		return gauge.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]))
	case 2:
		return gauge.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]))
	case 3:
		return gauge.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]), labelValue(labels, spec.labels[2]))
	case 4:
		return gauge.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]), labelValue(labels, spec.labels[2]), labelValue(labels, spec.labels[3]))
	case 5:
		return gauge.GetMetricWithLabelValues(labelValue(labels, spec.labels[0]), labelValue(labels, spec.labels[1]), labelValue(labels, spec.labels[2]), labelValue(labels, spec.labels[3]), labelValue(labels, spec.labels[4]))
	default:
		values := make([]string, len(spec.labels))
		for i, name := range spec.labels {
			values[i] = labelValue(labels, name)
		}
		return gauge.GetMetricWithLabelValues(values...)
	}
}

func labelValue(labels []Label, name string) string {
	for _, label := range labels {
		if label.Name == name {
			return label.Value
		}
	}
	return ""
}
