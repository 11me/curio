package curio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/markusmobius/go-trafilatura"
)

// PageFetcher gets the readable text of public web pages only. It resolves
// every host itself and connects only to public addresses, also after
// redirects, so a story link cannot reach the owner's network or this Mac.
type PageFetcher struct {
	Timeout  time.Duration
	MaxBytes int64                     // largest page body read, after decompression
	MinText  int                       // shorter text counts as no text
	Allow    func(netip.AddrPort) bool // nil means PublicAddr
	client   *http.Client
}

// NewPageFetcher makes a fetcher with a total time limit and a size limit.
func NewPageFetcher(timeout time.Duration, maxBytes int64) *PageFetcher {
	return &PageFetcher{Timeout: timeout, MaxBytes: maxBytes, MinText: 100}
}

var errBlockedAddr = errors.New("not a public address")

func (f *PageFetcher) allowed(ap netip.AddrPort) bool {
	if f.Allow != nil {
		return f.Allow(ap)
	}
	return PublicAddr(ap.Addr())
}

func (f *PageFetcher) httpClient() *http.Client {
	if f.client != nil {
		return f.client
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy: nil, // a proxy would connect for us and skip the address check
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("address %q: %w", addr, err)
			}
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("port %q: %w", portStr, err)
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
			for _, ip := range ips {
				ap := netip.AddrPortFrom(ip.Unmap(), uint16(port))
				if !f.allowed(ap) {
					continue
				}
				// connect to the checked address, not to the name again
				return dialer.DialContext(ctx, network, ap.String())
			}
			return nil, fmt.Errorf("%s: %w", host, errBlockedAddr)
		},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}
	f.client = &http.Client{
		Transport: transport,
		Timeout:   f.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s is not allowed", req.URL.Scheme)
			}
			return nil
		},
	}
	return f.client
}

// Text returns the article text of a public http or https page.
func (f *PageFetcher) Text(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse link: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("link %q: only http and https are fetched", truncate(rawURL, 80))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; curio/1.0; +https://github.com/11me/curio)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,text/plain;q=0.8")
	resp, err := f.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			return "", errBlockedAddr
		}
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch: status %d", resp.StatusCode)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/html") && !strings.Contains(ct, "xhtml") && !strings.Contains(ct, "text/plain") {
		return "", fmt.Errorf("fetch: content type %s is not a page", truncate(ct, 60))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read page: %w", err)
	}
	if int64(len(body)) > f.MaxBytes {
		return "", fmt.Errorf("page too large (over %d bytes)", f.MaxBytes)
	}
	extracted, err := trafilatura.Extract(bytes.NewReader(body), trafilatura.Options{
		EnableFallback: true, ExcludeComments: true, Deduplicate: true, OriginalURL: resp.Request.URL,
	})
	if err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}
	if extracted == nil {
		return "", errors.New("extract: no text")
	}
	text := strings.TrimSpace(extracted.ContentText)
	if len(text) < f.MinText {
		return "", fmt.Errorf("extract: text too short (%d characters)", len(text))
	}
	return text, nil
}

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "2002::/16",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// PublicAddr reports whether an address is a public unicast address: not
// loopback, private, link-local (cloud metadata), multicast, carrier NAT,
// documentation, NAT64 or 6to4.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
