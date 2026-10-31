package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"
)

// pprofAddress validates before listening, without resolving arbitrary DNS names.
func pprofAddress(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value == "1" || value == "true" {
		return "127.0.0.1:0", nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", err
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("pprof host must be loopback")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("invalid pprof port")
	}
	return net.JoinHostPort(host, port), nil
}

// startPprof returns the bound address and a completion signal, including on
// disabled/error paths. Profiling failures never prevent daemon startup.
func startPprof(ctx context.Context, value string, log *slog.Logger) (string, <-chan struct{}) {
	done := make(chan struct{})
	address, err := pprofAddress(value)
	if err != nil {
		log.Warn("invalid ZUT_DAEMON_PPROF", "err", err)
		close(done)
		return "", done
	}
	if address == "" {
		close(done)
		return "", done
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		log.Warn("pprof listen failed", "err", err)
		close(done)
		return "", done
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("pprof serve failed", "err", err)
		}
	}()
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-served:
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Warn("pprof shutdown failed", "err", err)
			_ = server.Close()
		}
		<-served
	}()
	log.Info("pprof listening", "address", ln.Addr().String())
	return ln.Addr().String(), done
}
