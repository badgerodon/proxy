package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	ConnectTimeout     time.Duration = 30 * time.Second
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
	cfg, err := GetConfig(filename)
	if err != nil {
		log.Println("error loading config:", err)
		return
	}
	this.configChannel <- cfg
}

func (this *proxy) start() {
	for {
		select {
		case cfg := <-this.configChannel:
			log.Println("load config")

			err := this.listen(cfg.Port)
			if err != nil {
				log.Println("error listening on", cfg.Port, ":", err)
				time.AfterFunc(RetryListenTimeout, func() {
					this.configChannel <- cfg
				})
				break
			}

			err = this.listenTLS(cfg.TLSPort)
			if err != nil {
				log.Println("error listening on", cfg.TLSPort, ":", err)
				time.AfterFunc(RetryListenTimeout, func() {
					this.configChannel <- cfg
				})
				break
			}

			this.backends = map[string]*backend{}
			for host, route := range cfg.Routes {
				be := &backend{route.Endpoints, 0}
				this.backends[host] = be
				log.Println(host, "-->", be.endpoints)
			}

			this.tlsConfigChannel <- this.getTLSConfig(cfg)

			this.config = cfg
		case ir := <-this.incomingRequestChannel:
			if ir.err != nil {
				ir.writeError(ir.err.Error(), 500)
			} else if be, ok := this.backends[ir.request.Host]; ok {
				endpoint := be.endpoints[be.count%uint64(len(be.endpoints))]
				go this.join(ir, endpoint)
				be.count++
			} else {
				ir.writeError(fmt.Sprintf("unknown host '%v'", ir.request.Host), 404)
			}
		}
	}
}

func (this *proxy) getTLSConfig(cfg *Config) *tlsConfig {
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{},
	}
	for host, route := range cfg.Routes {
		if route.TLS.Certificate != "" && route.TLS.Key != "" {
			certBs, err := base64.StdEncoding.DecodeString(route.TLS.Certificate)
			if err != nil {
				log.Println("invalid certificate for", host)
				continue
			}
			keyBs, err := base64.StdEncoding.DecodeString(route.TLS.Key)
			if err != nil {
				log.Println("invalid key for", host)
				continue
			}
			cert, err := tls.X509KeyPair(certBs, keyBs)
			if err != nil {
				log.Println("invalid tls for", host)
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

	go this.accept(l)
	this.frontends[port] = l
	this.config.TLSPort = port

	return nil
}

func (this *proxy) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("error accepting from", listener, ":", err)
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
				log.Println("error accepting from", listener, ":", err)
				break
			}
			conns <- conn
		}
		close(conns)
	}()

	config := <-this.tlsConfigChannel

	for {
		select {
		case cfg := <-this.tlsConfigChannel:
			config = cfg
		case conn, ok := <-conns:
			if !ok {
				return
			}
			conn = tls.Server(conn, config)
			go this.handleConn(conn)
		}
	}
}

func (this *proxy) handleConn(conn net.Conn) {
	ir := &incomingRequest{
		Conn:   conn,
		buffer: bytes.NewBuffer(nil),
	}
	rdr := io.TeeReader(ir.Conn, ir.buffer)
	ir.reader = bufio.NewReader(rdr)
	ir.request, ir.err = http.ReadRequest(ir.reader)
	this.incomingRequestChannel <- ir
	if ir.request != nil {
		log.Println(ir.request.Method, ir.request.URL)
	}
}

func (this *proxy) join(ir *incomingRequest, endpoint string) {
	conn, err := net.Dial("tcp", endpoint)
	if err != nil {
		log.Println("error connecting to", endpoint, ":", err)

		if ir.opened.Add(-ConnectTimeout).After(time.Now()) {
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
	if err != nil && err != io.EOF {
		log.Println("error copying buffer to", conn.RemoteAddr(), ":", err)
		return
	}

	var wg sync.WaitGroup
	halfJoin := func(dst net.Conn, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err != nil {
			log.Println("error copying from", src.RemoteAddr(), "to", dst.RemoteAddr(), ":", err)
		}
	}

	wg.Add(2)
	go halfJoin(ir.Conn, conn)
	go halfJoin(conn, ir.Conn)
	wg.Wait()
}
