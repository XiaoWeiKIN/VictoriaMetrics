package promscrape

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/chunkedbuffer"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httputil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/netutil"
)

var (
	maxResponseHeadersSize = flagutil.NewBytes("promscrape.maxResponseHeadersSize", 4096, "The maximum size of http response headers from Prometheus scrape targets")
	disableCompression     = flag.Bool("promscrape.disableCompression", false, "Whether to disable sending 'Accept-Encoding: gzip' request headers to all the scrape targets. "+
		"This may reduce CPU usage on scrape targets at the cost of higher network bandwidth utilization. "+
		"It is possible to set 'disable_compression: true' individually per each 'scrape_config' section in '-promscrape.config' for fine-grained control")
	disableKeepAlive = flag.Bool("promscrape.disableKeepAlive", false, "Whether to disable HTTP keep-alive connections when scraping all the targets. "+
		"This may be useful when targets has no support for HTTP keep-alive connection. "+
		"It is possible to set 'disable_keepalive: true' individually per each 'scrape_config' section in '-promscrape.config' for fine-grained control. "+
		"Note that disabling HTTP keep-alive may increase load on both vmagent and scrape targets")
	streamParse = flag.Bool("promscrape.streamParse", false, "Whether to enable stream parsing for metrics obtained from scrape targets. This may be useful "+
		"for reducing memory usage when millions of metrics are exposed per each scrape target. "+
		"It is possible to set 'stream_parse: true' individually per each 'scrape_config' section in '-promscrape.config' for fine-grained control")
)

type client struct {
	c                       *http.Client
	ctx                     context.Context
	scrapeURL               string
	scrapeTimeoutSecondsStr string
	setHeaders              func(req *http.Request) error
	setProxyHeaders         func(req *http.Request) error
	maxScrapeSize           int64
	disableCompression      bool
}

func newClient(ctx context.Context, sw *ScrapeWork) (*client, error) {
	ac := sw.AuthConfig
	setHeaders := func(req *http.Request) error {
		return sw.AuthConfig.SetHeaders(req, true)
	}
	setProxyHeaders := func(_ *http.Request) error {
		return nil
	}
	dialFunc := netutil.NewStatDialFunc("vm_promscrape")
	proxyURL := sw.ProxyURL
	var proxyURLFunc func(*http.Request) (*url.URL, error)

	if proxyURL != nil {
		// case for direct http proxy connection.
		// must be used for http based scrape targets
		// since standard golang http.transport has special case for it
		if strings.HasPrefix(sw.ScrapeURL, "http://") {
			if proxyURL.URL.Scheme == "https" {
				ac = sw.ProxyAuthConfig
			}
			proxyURLFunc = http.ProxyURL(proxyURL.URL)
			setProxyHeaders = func(req *http.Request) error {
				return proxyURL.SetHeaders(sw.ProxyAuthConfig, req)
			}
		} else {
			// HTTP-Connect or socks5 proxy tunnel
			// it makes possible to use separate tls configurations
			// for proxy and backend connections
			proxyDial, err := proxyURL.NewDialFunc(sw.ProxyAuthConfig)
			if err != nil {
				return nil, fmt.Errorf("cannot create dialer for proxy_url=%q connection: %w", proxyURL, err)
			}
			dialFunc = netutil.NewStatDialFuncWithDial("vm_promscrape", proxyDial)
		}
	}

	tr := httputil.NewTransport(false, "vm_promscrape")
	tr.Proxy = proxyURLFunc
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.IdleConnTimeout = 2 * sw.ScrapeInterval
	tr.DisableKeepAlives = *disableKeepAlive || sw.DisableKeepAlive
	tr.DialContext = dialFunc
	tr.MaxIdleConnsPerHost = 100
	tr.MaxResponseHeaderBytes = int64(maxResponseHeadersSize.N)

	// The client handles response compression manually in order to optimize it.
	// See client.ReadData
	tr.DisableCompression = true

	hc := &http.Client{
		Transport: ac.NewRoundTripper(tr),
		Timeout:   sw.ScrapeTimeout,
	}
	if sw.DenyRedirects {
		hc.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	c := &client{
		c:                       hc,
		ctx:                     ctx,
		scrapeURL:               sw.ScrapeURL,
		scrapeTimeoutSecondsStr: fmt.Sprintf("%.3f", sw.ScrapeTimeout.Seconds()),
		setHeaders:              setHeaders,
		setProxyHeaders:         setProxyHeaders,
		maxScrapeSize:           sw.MaxScrapeSize,
		disableCompression:      *disableCompression || sw.DisableCompression,
	}
	return c, nil
}

func (c *client) ReadData(dst *chunkedbuffer.Buffer) (bool, error) {
	deadline := time.Now().Add(c.c.Timeout)
	ctx, cancel := context.WithDeadline(c.ctx, deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scrapeURL, nil)
	if err != nil {
		return false, fmt.Errorf("cannot create request for %q: %w", c.scrapeURL, err)
	}
	// The following `Accept` header has been copied from Prometheus sources.
	// See https://github.com/prometheus/prometheus/blob/f9d21f10ecd2a343a381044f131ea4e46381ce09/scrape/scrape.go#L532 .
	// This is needed as a workaround for scraping stupid Java-based servers such as Spring Boot.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/608 for details.
	// Do not bloat the `Accept` header with OpenMetrics shit, since it looks like dead standard now.
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	// Set X-Prometheus-Scrape-Timeout-Seconds like Prometheus does, since it is used by some exporters such as PushProx.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/1179#issuecomment-813117162
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", c.scrapeTimeoutSecondsStr)
	req.Header.Set("User-Agent", "vm_promscrape")
	if err := c.setHeaders(req); err != nil {
		return false, fmt.Errorf("failed to set request headers for %q: %w", c.scrapeURL, err)
	}
	if err := c.setProxyHeaders(req); err != nil {
		return false, fmt.Errorf("failed to set proxy request headers for %q: %w", c.scrapeURL, err)
	}
	if !c.disableCompression {
		req.Header.Set("Accept-Encoding", "gzip")
	}

	scrapeRequests.Inc()
	resp, err := c.c.Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok && ue.Timeout() {
			scrapesTimedout.Inc()
		}
		return false, fmt.Errorf("cannot perform request to %q: %w", c.scrapeURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		metrics.GetOrCreateCounter(fmt.Sprintf(`vm_promscrape_scrapes_total{status_code="%d"}`, resp.StatusCode)).Inc()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			respBody = []byte(err.Error())
		}
		return false, fmt.Errorf("unexpected status code returned when scraping %q: %d; expecting %d; response body: %q",
			c.scrapeURL, resp.StatusCode, http.StatusOK, respBody)
	}
	scrapesOK.Inc()

	// Read the data from resp.Body
	r := io.LimitReader(resp.Body, c.maxScrapeSize)
	_, err = dst.ReadFrom(r)
	if err != nil {
		if ue, ok := err.(*url.Error); ok && ue.Timeout() {
			scrapesTimedout.Inc()
		}
		return false, fmt.Errorf("cannot read data from %s: %w", c.scrapeURL, err)
	}
	if int64(dst.Len()) >= c.maxScrapeSize {
		maxScrapeSizeExceeded.Inc()
		return false, fmt.Errorf("the response from %q exceeds -promscrape.maxScrapeSize or max_scrape_size in the scrape config (%d bytes). "+
			"Possible solutions are: reduce the response size for the target, increase -promscrape.maxScrapeSize command-line flag, "+
			"increase max_scrape_size value in scrape config for the given target", c.scrapeURL, c.maxScrapeSize)
	}

	isGzipped := resp.Header.Get("Content-Encoding") == "gzip"
	return isGzipped, nil
}

var (
	maxScrapeSizeExceeded = metrics.NewCounter(`vm_promscrape_max_scrape_size_exceeded_errors_total`)
	scrapesTimedout       = metrics.NewCounter(`vm_promscrape_scrapes_timed_out_total`)
	scrapesOK             = metrics.NewCounter(`vm_promscrape_scrapes_total{status_code="200"}`)
	scrapeRequests        = metrics.NewCounter(`vm_promscrape_scrape_requests_total`)
)
