package caddybgp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func init() {
	caddy.RegisterModule(HTTPProbe{})
}

const probeBodyLimit = 64 << 10

type HTTPProbe struct {
	URL           Value       `json:"url"`
	Method        string      `json:"method,omitempty"`
	Host          string      `json:"host,omitempty"`
	Headers       http.Header `json:"headers,omitempty"`
	Status        []string    `json:"status,omitempty"`
	TLSServerName string      `json:"tls_server_name,omitempty"`
	TLSInsecure   bool        `json:"tls_insecure,omitempty"`
	TLSRootCA     string      `json:"tls_ca,omitempty"`

	url    string
	client *http.Client
}

func (HTTPProbe) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "bgp.probes.http", New: func() caddy.Module { return new(HTTPProbe) }}
}

func (p *HTTPProbe) Provision(caddy.Context) error {
	replacer := caddy.NewReplacer()
	address, err := p.URL.resolve(replacer)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}

	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}

	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("url %q must be an absolute http or https URL", address)
	}

	if len(p.Status) == 0 {
		p.Status = []string{"2xx"}
	}

	for _, status := range p.Status {
		if err := validateStatus(status); err != nil {
			return err
		}
	}

	settings := &tls.Config{ServerName: p.TLSServerName, InsecureSkipVerify: p.TLSInsecure, MinVersion: tls.VersionTLS12}
	if p.TLSRootCA != "" {
		if settings.RootCAs, err = loadRoots(p.TLSRootCA); err != nil {
			return err
		}
	}

	p.url = parsed.String()
	p.client = &http.Client{
		Transport:     &http.Transport{TLSClientConfig: settings, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return nil
}

func loadRoots(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tls_ca: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("tls_ca: no certificate in %s", path)
	}

	return pool, nil
}

func validateStatus(status string) error {
	digits := strings.TrimSuffix(status, "xx")
	number, err := strconv.Atoi(digits)
	if err != nil {
		return fmt.Errorf("status %q must be a code such as 200 or a class such as 2xx", status)
	}

	if strings.HasSuffix(status, "xx") {
		number *= 100
	}

	if number < 100 || number > 599 {
		return fmt.Errorf("status %q is out of range", status)
	}

	return nil
}

func (p *HTTPProbe) Probe(ctx context.Context) error {
	method := p.Method
	if method == "" {
		method = http.MethodGet
	}

	request, err := http.NewRequestWithContext(ctx, method, p.url, http.NoBody)
	if err != nil {
		return err
	}

	request.Header.Set("User-Agent", "caddy-bgp")
	for name, values := range p.Headers {
		request.Header[http.CanonicalHeaderKey(name)] = values
	}

	if p.Host != "" {
		request.Host = p.Host
	}

	response, err := p.client.Do(request)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(io.Discard, io.LimitReader(response.Body, probeBodyLimit))
	if err := errors.Join(copyErr, response.Body.Close()); err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if !p.accepts(response.StatusCode) {
		return fmt.Errorf("unexpected status %d", response.StatusCode)
	}

	return nil
}

func (p *HTTPProbe) accepts(code int) bool {
	text := strconv.Itoa(code)
	for _, status := range p.Status {
		if status == text || (strings.HasSuffix(status, "xx") && status[:1] == text[:1]) {
			return true
		}
	}

	return false
}

func (p *HTTPProbe) String() string {
	return "http " + p.url
}

func (p *HTTPProbe) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	arguments := d.RemainingArgs()
	if len(arguments) != 1 {
		return d.ArgErr()
	}

	p.URL = Value(arguments[0])
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if err := p.unmarshalOption(d); err != nil {
			return err
		}
	}

	return nil
}

func (p *HTTPProbe) unmarshalOption(d *caddyfile.Dispenser) error {
	option := d.Val()
	arguments := d.RemainingArgs()
	switch option {
	case "status":
		if len(arguments) == 0 {
			return d.ArgErr()
		}

		p.Status = append(p.Status, arguments...)
		return nil
	case "header":
		if len(arguments) != 2 {
			return d.ArgErr()
		}

		if p.Headers == nil {
			p.Headers = make(http.Header)
		}

		p.Headers.Add(arguments[0], arguments[1])
		return nil
	case "tls_insecure":
		p.TLSInsecure = true
		if len(arguments) != 0 {
			return d.ArgErr()
		}

		return nil
	}

	if len(arguments) != 1 {
		return d.ArgErr()
	}

	switch option {
	case "method":
		p.Method = arguments[0]
	case "host":
		p.Host = arguments[0]
	case "tls_server_name":
		p.TLSServerName = arguments[0]
	case "tls_ca":
		p.TLSRootCA = arguments[0]
	default:
		return d.Errf("unknown http check option %q", option)
	}

	return nil
}

var (
	_ caddy.Provisioner     = (*HTTPProbe)(nil)
	_ caddyfile.Unmarshaler = (*HTTPProbe)(nil)
	_ Prober                = (*HTTPProbe)(nil)
)
