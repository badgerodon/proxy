package proxy

import (
	"fmt"
	"github.com/howeyc/fsnotify"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func NewRoundRobinReverseProxy(endpoints []string) (*httputil.ReverseProxy, error) {
	targets := make([]*url.URL, len(endpoints))
	for i, endpoint := range endpoints {
		var err error
		targets[i], err = url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
	}

	var prev uint64
	director := func(req *http.Request) {
		target := targets[int(atomic.AddUint64(&prev, 1)%uint64(len(targets)))]
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		if target.RawQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = target.RawQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
		}
	}
	return &httputil.ReverseProxy{Director: director}, nil
}

type (
	Proxy struct {
		config         chan *Config
		public, secure net.Listener
		watcher        *fsnotify.Watcher
		handlers       map[string]http.Handler
		lock           sync.Mutex
	}
)

func NewProxy(filename string) (*Proxy, error) {
	this := Proxy{
		config: make(chan *Config, 1),
	}
	var err error
	this.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			select {
			case evt := <-this.watcher.Event:
				if evt.IsModify() && filepath.Base(evt.Name) == filepath.Base(filename) {
					this.Reload(filename)
				}
			case err := <-this.watcher.Error:
				log.Println("ERROR", err)
			}
		}
	}()
	err = this.watcher.Watch(filepath.Dir(filename))
	if err != nil {
		this.watcher.Close()
		return nil, err
	}
	this.Reload(filename)
	return &this, nil
}

// Reload the config
func (this *Proxy) Reload(filename string) error {
	cfg, err := GetConfig(filename)
	if err != nil {
		return err
	}
	select {
	case <-this.config:
	default:
	}
	this.config <- cfg
	return nil
}

func (this *Proxy) Close() {
	fmt.Println("Close")
	if this.public != nil {
		this.public.Close()
		this.public = nil
	}
	if this.secure != nil {
		this.secure.Close()
		this.secure = nil
	}
	if this.watcher != nil {
		this.watcher.Close()
		this.watcher = nil
	}
	if this.config != nil {
		close(this.config)
		this.config = nil
	}
}

func (this *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	this.lock.Lock()
	handler, ok := this.handlers[r.Host]
	if !ok && strings.Contains(r.Host, ":") {
		handler, ok = this.handlers[r.Host[:strings.Index(r.Host, ":")]]
	}
	this.lock.Unlock()

	if !ok {
		http.Error(w, "Not Found", 404)
		return
	}

	handler.ServeHTTP(w, r)
}

func (this *Proxy) ListenAndServe() error {
	var prev *Config
	var err error

	for {
		select {
		case cfg, ok := <-this.config:
			// Closed
			if !ok {
				return nil
			}

			log.Println("Reload config: ", cfg)

			handlers := make(map[string]http.Handler)
			for host, route := range cfg.Routes {
				handlers[host], err = NewRoundRobinReverseProxy(route.Endpoints)
				if err != nil {
					this.Close()
					return err
				}
			}
			this.lock.Lock()
			this.handlers = handlers
			this.lock.Unlock()

			if prev == nil || prev.Port != cfg.Port {
				if this.public != nil {
					this.public.Close()
				}
				this.public, err = net.Listen("tcp", fmt.Sprint(":", cfg.Port))
				if err != nil {
					this.Close()
					return err
				}
				go http.Serve(this.public, this)
			}
			if prev == nil || prev.SSLPort != cfg.SSLPort {
				// TODO: setup SSL
			}
			prev = cfg
		}
	}
}
