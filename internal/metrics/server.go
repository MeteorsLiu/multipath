package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	listener net.Listener
	server   *http.Server
}

func NewServer(listenAddr string) (*Server, error) {
	listenAddr = strings.TrimSpace(listenAddr)
	if listenAddr == "" || listenAddr == "off" || listenAddr == "false" || listenAddr == "disabled" {
		return nil, nil
	}

	listener, err := listenTCP(listenAddr)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(Default.Gatherer(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("multipath metrics: GET /metrics\n"))
			return
		}
		http.NotFound(w, r)
	})

	return &Server{
		listener: listener,
		server: &http.Server{
			Handler: mux,
		},
	}, nil
}

func listenTCP(listenAddr string) (net.Listener, error) {
	network := "tcp"
	host, _, err := net.SplitHostPort(listenAddr)
	if err == nil && net.ParseIP(host).To4() != nil {
		network = "tcp4"
	}
	return net.Listen(network, listenAddr)
}

func (s *Server) Addr() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) Run(ctx context.Context) error {
	if s == nil || s.server == nil || s.listener == nil {
		<-ctx.Done()
		return ctx.Err()
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.server.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		_ = s.server.Close()
		err := <-errCh
		if err != nil {
			return err
		}
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Server) Close() error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Close()
}
