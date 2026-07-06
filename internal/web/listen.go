package web

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"

	"github.com/erazumov/wgserver/internal/config"
)

// AdminListener bundles the listener and optional TLS config for the
// admin UI. Exactly one of TLSServer (HTTPS) or Server (plain) is
// non-nil and ready to serve on Listener. For UNIX sockets TLSServer
// is always nil (TLS over UDS is rarely useful and complicates the
// install path).
type AdminListener struct {
	Listener  net.Listener
	TLSServer *tls.Config // nil for plain HTTP / UNIX
}

// ListenAdmin binds the admin UI per the HTTP config. Selection order:
//  1. UnixSocket non-empty → UNIX socket
//  2. TLSCertFile + TLSKeyFile → TLS on TCP
//  3. otherwise → plain HTTP on TCP (Addr)
//
// For UNIX sockets, the socket file is removed if it already exists
// (stale after a crash) and chmod'd to 0660 so the wgserver group can
// read it. The caller is responsible for ensuring the parent dir
// exists; install.sh creates /var/run/wgserver with mode 0750.
func ListenAdmin(cfg config.HTTPConfig) (*AdminListener, error) {
	if cfg.UnixSocket != "" {
		return listenUnix(cfg.UnixSocket)
	}
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return nil, fmt.Errorf("http.tls_cert_file and http.tls_key_file must be set together")
		}
		l, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("listen tcp %s: %w", cfg.Addr, err)
		}
		tlsCfg, err := loadTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("load tls: %w", err)
		}
		return &AdminListener{Listener: l, TLSServer: tlsCfg}, nil
	}
	l, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen tcp %s: %w", cfg.Addr, err)
	}
	return &AdminListener{Listener: l}, nil
}

func listenUnix(path string) (*AdminListener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod socket %s: %w", path, err)
	}
	return &AdminListener{Listener: l}, nil
}

// ListenHealth returns a plain TCP listener for the /healthz endpoint.
// Always plain HTTP so the auto-updater can hit it without TLS.
func ListenHealth(addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen tcp %s: %w", addr, err)
	}
	return l, nil
}

func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
