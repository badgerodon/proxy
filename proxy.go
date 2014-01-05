package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	ConnectTimeout     time.Duration = 10 * time.Second
	RetryListenTimeout time.Duration = 5 * time.Second
)

type (
	proxy struct {
		config    *Config
		frontends map[int]net.Listener
		backends  map[string]*backend

		configChannel          chan *Config
		incomingRequestChannel chan *incomingRequest
		tlsConfigChannel       chan *tls.Config
	}
	backend struct {
		endpoints []string
		count     uint64
	}
)

func (this backend) String() string {
	return fmt.Sprint(this.endpoints)
}

func newProxy() *proxy {
	return &proxy{
		config:                 &Config{},
		frontends:              make(map[int]net.Listener),
		backends:               make(map[string]*backend),
		configChannel:          make(chan *Config),
		incomingRequestChannel: make(chan *incomingRequest),
		tlsConfigChannel:       make(chan *tls.Config),
	}
}

func (this *proxy) reload(filename string) {
	info("reload: %v", filename)

	cfg, err := GetConfig(filename)
	if err != nil {
		warn("reload error: %v", err)
		return
	}
	this.configChannel <- cfg
}

func (this *proxy) start() {
	for {
		select {
		case cfg := <-this.configChannel:
			info("new config")
			err := this.listen(cfg.Port)
			if err != nil {
				warn("listen error on port %v: %v", cfg.Port, err)
				time.AfterFunc(RetryListenTimeout, func() {
					this.configChannel <- cfg
				})
				break
			}

			err = this.listenTLS(cfg.TLSPort)
			if err != nil {
				warn("listenTLS error on port %v: %v", cfg.TLSPort, err)
				time.AfterFunc(RetryListenTimeout, func() {
					this.configChannel <- cfg
				})
				break
			}

			this.backends = map[string]*backend{}
			for host, route := range cfg.Routes {
				be := &backend{route.Endpoints, 0}
				this.backends[host] = be
			}
			info("routes: %v", this.backends)

			this.tlsConfigChannel <- this.getTLSConfig(cfg)

			this.config = cfg
		case ir := <-this.incomingRequestChannel:
			if ir.err != nil {
				if ir.err != io.EOF {
					warn("request error: %v", ir.err)
					go ir.writeError(ir.err.Error(), 500)
				} else {
					ir.Conn.Close()
				}
			} else if be, ok := this.backends[ir.request.Host]; ok {
				endpoint := be.endpoints[be.count%uint64(len(be.endpoints))]
				go this.join(ir, endpoint)
				be.count++
			} else {
				warn("unknown host: %v", ir.request.Host)
				go ir.writeError("unknown host", 404)
			}
		}
	}
}

func (this *proxy) getTLSConfig(cfg *Config) *tls.Config {
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{},
	}
	for host, route := range cfg.Routes {
		if route.TLS.Certificate != "" && route.TLS.Key != "" {
			certBs, err := base64.StdEncoding.DecodeString(route.TLS.Certificate)
			if err != nil {
				warn("invalid certificate for %v", host)
				continue
			}
			keyBs, err := base64.StdEncoding.DecodeString(route.TLS.Key)
			if err != nil {
				warn("invalid key for %v", host)
				continue
			}
			cert, err := tls.X509KeyPair(certBs, keyBs)
			if err != nil {
				warn("invalid tls for %v", host)
				continue
			}
			tlsConfig.Certificates = append(tlsConfig.Certificates, cert)
		}
	}
	tlsConfig.BuildNameToCertificate()
	return tlsConfig
}

func (this *proxy) listen(port int) error {
	if port == this.config.Port {
		return nil
	}

	l, ok := this.frontends[this.config.Port]
	if ok {
		l.Close()
		delete(this.frontends, this.config.Port)
	}

	l, err := net.Listen("tcp", fmt.Sprint(":", port))
	if err != nil {
		return err
	}

	go this.accept(l)
	this.frontends[port] = l
	this.config.Port = port

	return nil
}

func (this *proxy) listenTLS(port int) error {
	if port == this.config.TLSPort {
		return nil
	}

	l, ok := this.frontends[this.config.TLSPort]
	if ok {
		l.Close()
		delete(this.frontends, this.config.TLSPort)
	}

	l, err := net.Listen("tcp", fmt.Sprint(":", port))
	if err != nil {
		return err
	}

	go this.acceptTLS(l)
	this.frontends[port] = l
	this.config.TLSPort = port

	return nil
}

func (this *proxy) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go this.handleConn(conn)
	}
}

func (this *proxy) acceptTLS(listener net.Listener) {
	conns := make(chan net.Conn)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				break
			}
			conns <- conn
		}
		close(conns)
	}()

	config := &tls.Config{}

	for {
		select {
		case cfg := <-this.tlsConfigChannel:
			info("updated tls configuration")
			config = cfg
		case conn, ok := <-conns:
			if !ok {
				return
			}
			if len(config.Certificates) == 0 {
				warn("no certificates configured")
				conn.Close()
			} else {
				conn = tls.Server(conn, config)
				go this.handleConn(conn)
			}
		}
	}
}

func (this *proxy) handleConn(conn net.Conn) {
	info("incoming from %v", conn.RemoteAddr())
	ir := &incomingRequest{
		Conn:   conn,
		buffer: bytes.NewBuffer(nil),
		opened: time.Now(),
	}
	rdr := io.TeeReader(ir.Conn, ir.buffer)
	ir.request, ir.err = http.ReadRequest(bufio.NewReader(rdr))
	if ir.request != nil {
		info("%v %v", ir.request.Method, ir.request.URL)
	}
	this.incomingRequestChannel <- ir
}

func (this *proxy) join(ir *incomingRequest, endpoint string) {
	conn, err := net.DialTimeout("tcp", endpoint, ConnectTimeout)
	if err != nil {
		warn("join error to %v: %v", endpoint, err)

		if ir.opened.Add(ConnectTimeout).Before(time.Now()) {
			ir.writeError("timeout", 504)
		} else {
			time.AfterFunc(time.Second, func() {
				this.incomingRequestChannel <- ir
			})
		}

		return
	}

	defer ir.Conn.Close()
	defer conn.Close()

	_, err = io.Copy(conn, ir.buffer)
	if err != nil {
		warn("join copy buffer error: %v", err)
		return
	}

	var wg sync.WaitGroup
	halfJoin := func(dst net.Conn, src net.Conn) {
		defer wg.Done()
		defer dst.Close()
		defer src.Close()
		_, err := io.Copy(dst, src)
		if err != nil {
			warn("join copy error: %v", err)
		}
	}

	wg.Add(2)
	go halfJoin(ir.Conn, conn)
	go halfJoin(conn, ir.Conn)
	wg.Wait()
}
