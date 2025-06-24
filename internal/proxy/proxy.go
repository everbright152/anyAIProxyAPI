package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Proxy proxy struct
type Proxy struct {
	port     string
	proxyURL string
	server   *http.Server
}

// createProxyDialer creates a dialer for the specified proxy URL
func createProxyDialer(proxyURL string) (proxy.Dialer, error) {
	if proxyURL == "" {
		return proxy.Direct, nil
	}

	proxyURLParsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %v", err)
	}

	var dialer proxy.Dialer
	switch proxyURLParsed.Scheme {
	case "http", "https":
		dialer = &httpProxyDialer{proxyURL: proxyURLParsed}
	case "socks5":
		auth := &proxy.Auth{}
		if proxyURLParsed.User != nil {
			auth.User = proxyURLParsed.User.Username()
			auth.Password, _ = proxyURLParsed.User.Password()
			dialer, err = proxy.SOCKS5("tcp", proxyURLParsed.Host, auth, proxy.Direct)
		} else {
			dialer, err = proxy.SOCKS5("tcp", proxyURLParsed.Host, nil, proxy.Direct)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURLParsed.Scheme)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create proxy dialer: %v", err)
	}

	return dialer, nil
}

// httpProxyDialer implements proxy.Dialer for HTTP proxies
type httpProxyDialer struct {
	proxyURL *url.URL
}

// Dial connects to the address through the HTTP proxy
func (d *httpProxyDialer) Dial(_, addr string) (net.Conn, error) {
	connectReq := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}

	if d.proxyURL.User != nil {
		password, _ := d.proxyURL.User.Password()
		auth := d.proxyURL.User.Username() + ":" + password
		basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
		connectReq.Header.Set("Proxy-Authorization", basicAuth)
	}

	conn, err := net.Dial("tcp", d.proxyURL.Host)
	if err != nil {
		return nil, err
	}

	if err = connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	respReader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(respReader, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != 200 {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy error: %s", resp.Status)
	}

	return conn, nil
}

// NewProxy create a new proxy object
func NewProxy(port, proxyURL string) *Proxy {
	return &Proxy{
		port:     port,
		proxyURL: proxyURL,
	}
}

// Start Start proxy server
func (p *Proxy) Start() error {
	addr := ":" + p.port
	p.server = &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				p.handleHTTPS(w, r)
			} else {
				p.handleHTTP(w, r)
			}
		}),
	}

	log.Debugf("Proxy server started on %s", addr)
	return p.server.ListenAndServe()
}

func (p *Proxy) Close() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}

// handleHTTP handle HTTP request
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// check proxy config
	if p.proxyURL != "" {
		// create custom transport
		transport := &http.Transport{}

		// parse URL
		parsedURL, err := url.Parse(p.proxyURL)
		if err != nil {
			// log.Debugf("Failed to parse proxy URL: %v", err)
			http.Error(w, "Proxy configuration error", http.StatusInternalServerError)
			return
		}

		// set transport type by schema
		switch parsedURL.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsedURL)
		case "socks4", "socks5":
			// create proxy dialer
			dialer, errCreateProxyDialer := createProxyDialer(p.proxyURL)
			if errCreateProxyDialer != nil {
				// log.Debugf("Failed to create proxy dialer: %v", errCreateProxyDialer)
				http.Error(w, "Proxy configuration error", http.StatusInternalServerError)
				return
			}

			// set custom dialer
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		default:
			// log.Debugf("Unsupported proxy scheme: %s", parsedURL.Scheme)
			http.Error(w, "Unsupported proxy scheme", http.StatusInternalServerError)
			return
		}

		// use custom Transport send request
		resp, err := transport.RoundTrip(r)
		if err != nil {
			// log.Debugf("Failed to send request through proxy: %v", err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		// copy response header
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		// copy response body
		_, _ = io.Copy(w, resp.Body)
	} else {
		// Use default Transport
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		// copy response header
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		// copy response body
		_, _ = io.Copy(w, resp.Body)
	}
}

// handleHTTPS handle HTTPS request
func (p *Proxy) handleHTTPS(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if !strings.Contains(host, ":") {
		host = host + ":443"
	}

	p.handleDirectHTTPS(w, r, host)
}

// handleDirectHTTPS handle direct HTTPS request
func (p *Proxy) handleDirectHTTPS(w http.ResponseWriter, _ *http.Request, host string) {
	var targetConn net.Conn
	var err error

	// check proxy config
	proxyURL := p.proxyURL
	if proxyURL != "" {
		// create proxy dialer
		dialer, errCreateProxyDialer := createProxyDialer(proxyURL)
		if errCreateProxyDialer != nil {
			// log.Debugf("Failed to create proxy dialer: %v", errCreateProxyDialer)
			http.Error(w, "Proxy configuration error", http.StatusInternalServerError)
			return
		}

		// use proxy connect to target server
		targetConn, err = dialer.Dial("tcp", host)
		if err != nil {
			// log.Debugf("Failed to connect to target server through proxy: %v", err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	} else {
		// direct connect to target server
		targetConn, err = net.Dial("tcp", host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	defer func() {
		_ = targetConn.Close()
	}()

	// Client connection established notification
	w.WriteHeader(http.StatusOK)

	// Get the raw connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacker is unsupported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer func() {
		_ = clientConn.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// Read Client -> Write Server
	go func() {
		defer wg.Done()
		clientReader := bufio.NewReader(clientConn)
		clientBuf := make([]byte, 4096)

		for {
			n, errRead := clientReader.Read(clientBuf)
			if errRead != nil {
				if errRead != io.EOF {
					if !strings.Contains(errRead.Error(), "use of closed network connection") {
						// log.Errorf("Failed to read client data: %v", errRead)
					}
				}
				break
			}

			// forward to server
			_, err = targetConn.Write(clientBuf[:n])
			if err != nil {
				// log.Debugf("Failed to write server data: %v", err)
				break
			}
		}

		_ = targetConn.(*net.TCPConn).CloseWrite()
	}()

	// Read Server -> Write Client
	go func() {
		defer wg.Done()
		serverReader := bufio.NewReader(targetConn)
		serverBuf := make([]byte, 4096)

		for {
			n, errRead := serverReader.Read(serverBuf)
			if errRead != nil {
				if errRead != io.EOF {
					// log.Debugf("Failed to read server data: %v", errRead)
				}
				break
			}

			// forward to client
			_, errWrite := clientConn.Write(serverBuf[:n])
			if errWrite != nil {
				// log.Debugf("Failed to write client data: %v", errWrite)
				break
			}
		}

		_ = clientConn.(*net.TCPConn).CloseWrite()
	}()

	wg.Wait()
}
