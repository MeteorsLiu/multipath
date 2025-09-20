package prom

import (
	"fmt"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	UDPTraffic = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "node",
		Name:      "udp_traffic_total",
		Help:      "The total UDP traffic of node receiver and sender",
	}, []string{"addr"})
	TCPTraffic = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "node",
		Name:      "tcp_traffic_total",
		Help:      "The total TCP traffic of node receiver and sender",
	}, []string{"addr"})
	ProbeRtt = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "prober",
		Name:      "rtt",
		Help:      "The RTT measured time",
	}, []string{"addr"})
	ProbeRttPredict = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "prober",
		Name:      "rtt_predict",
		Help:      "The predicted RTT time",
	}, []string{"addr"})
	ProbeInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "prober",
		Name:      "inflight",
		Help:      "The number of prober packets in flight",
	}, []string{"addr"})
	ProbeState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "prober",
		Name:      "state",
		Help:      "The state of the prober",
	}, []string{"addr"})

	ProbeNextTimout = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "prober",
		Name:      "next_timeout_ms",
		Help:      "The deadline of probe next timeout",
	}, []string{"addr"})
	NodeReconnectNum = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "multipath",
		Subsystem: "node",
		Name:      "node_reconnect_total",
		Help:      "The total number of node reconnections",
	}, []string{"addr"})
)

func Register() {
	prometheus.MustRegister(UDPTraffic)
	prometheus.MustRegister(TCPTraffic)
	prometheus.MustRegister(ProbeRtt)
	prometheus.MustRegister(ProbeRttPredict)
	prometheus.MustRegister(ProbeInflight)
	prometheus.MustRegister(ProbeState)
	prometheus.MustRegister(ProbeNextTimout)
	prometheus.MustRegister(NodeReconnectNum)
}

func SetupServer(listenAddr string) {
	Register()
	http.Handle("/metrics", promhttp.Handler())
	fmt.Printf("Prometheus exporter listen on: %s\n", listenAddr)

	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Println("prom: ", err)
		return
	}
	go http.Serve(l, nil)
}
