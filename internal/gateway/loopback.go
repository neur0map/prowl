package gateway

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
)

// DefaultPort is where the gateway listens: 8788, deliberately adjacent to
// (not equal to) Prowl's 8787, so a machine can run both gateways side by
// side and neither silently shadows the other. It binds loopback only: the
// process holds every provider key the user owns, so it must not be reachable
// from the network.
const DefaultPort = 8788

// ListenLoopback binds the gateway's port on both loopback families without
// serving, so a caller can learn the address before traffic starts.
//
// Both families are bound. On a host where `localhost` resolves to ::1 first
// -- which is the default on this distro -- an IPv4-only bind means a browser
// typing localhost:8787 is refused while 127.0.0.1:8787 works, and the
// dashboard looks broken for no visible reason. The IPv6 bind is optional: a
// kernel with IPv6 disabled still gets a working gateway.
func ListenLoopback(port int) (net.Listener, error) {
	if port == 0 {
		port = DefaultPort
	}
	v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("gateway cannot bind 127.0.0.1:%d: %w", port, err)
	}

	v6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		slog.Debug("Gateway serving IPv4 loopback only", "error", err)
		return v4, nil
	}
	return &pairListener{primary: v4, secondary: v6}, nil
}

// pairListener accepts from two listeners as one, so a single http.Serve call
// covers both loopback families.
type pairListener struct {
	primary   net.Listener
	secondary net.Listener

	once   sync.Once
	accept chan acceptResult
	closed chan struct{}
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func (p *pairListener) start() {
	p.accept = make(chan acceptResult)
	p.closed = make(chan struct{})
	for _, ln := range []net.Listener{p.primary, p.secondary} {
		go func(ln net.Listener) {
			for {
				conn, err := ln.Accept()
				select {
				case p.accept <- acceptResult{conn: conn, err: err}:
				case <-p.closed:
					if conn != nil {
						_ = conn.Close()
					}
					return
				}
				if err != nil {
					return
				}
			}
		}(ln)
	}
}

func (p *pairListener) Accept() (net.Conn, error) {
	p.once.Do(p.start)
	select {
	case res := <-p.accept:
		return res.conn, res.err
	case <-p.closed:
		return nil, net.ErrClosed
	}
}

func (p *pairListener) Close() error {
	p.once.Do(p.start)
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
	err := p.primary.Close()
	if err2 := p.secondary.Close(); err == nil {
		err = err2
	}
	return err
}

// Addr reports the IPv4 loopback address, which is what the dashboard URL and
// the registered provider base URL use.
func (p *pairListener) Addr() net.Addr { return p.primary.Addr() }
