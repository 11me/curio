package curio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const articleHTML = `<html><head><title>T</title></head><body><article><h1>Title</h1>
<p>Rust 1.100 ships a faster compiler, better error messages and a new borrow checker mode for large projects.</p>
<p>The release also stabilizes several library functions and improves incremental builds on all platforms.</p>
</article></body></html>`

// noLocalAddrs lets tests reach httptest servers on loopback.
func noLocalAddrs() ([]netip.Addr, error) { return nil, nil }

func portOf(t *testing.T, srv *httptest.Server) uint16 {
	t.Helper()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	p, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return uint16(p)
}

func TestPublicAddr(t *testing.T) {
	tests := map[string]bool{
		"93.184.216.34":    true,
		"2606:4700::1111":  true,
		"127.0.0.1":        false,
		"10.1.2.3":         false,
		"172.16.0.1":       false,
		"192.168.1.10":     false,
		"169.254.169.254":  false, // cloud metadata
		"100.64.0.1":       false, // carrier NAT, tailnets
		"0.0.0.0":          false,
		"224.0.0.1":        false,
		"255.255.255.255":  false,
		"198.18.0.1":       false,
		"::1":              false,
		"fe80::1":          false,
		"fd00::1":          false,
		"::ffff:127.0.0.1": false,
		"::ffff:10.0.0.1":  false,
		"64:ff9b::a00:1":   false, // NAT64 of 10.0.0.1
		"ff02::1":          false,
	}
	for ip, want := range tests {
		assert.Equal(t, want, PublicAddr(netip.MustParseAddr(ip)), ip)
	}
}

func TestPageFetcher_RefusesPrivateAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer srv.Close()
	f := NewPageFetcher(5*time.Second, 1<<20)
	_, err := f.Text(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a public address")
}

func TestPageFetcher_RefusesRedirectToBlockedAddress(t *testing.T) {
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer private.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/secret", http.StatusFound)
	}))
	defer public.Close()

	f := NewPageFetcher(5*time.Second, 1<<20)
	allowed := portOf(t, public)
	f.Allow = func(ap netip.AddrPort) bool { return ap.Port() == allowed }
	f.LocalAddrs = noLocalAddrs
	_, err := f.Text(context.Background(), public.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a public address")
}

func TestPageFetcher_RefusesOtherSchemesAndHugePages(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>" + strings.Repeat("a", 4096) + "</p></body></html>"))
	}))
	defer big.Close()
	f := NewPageFetcher(5*time.Second, 1024)
	f.Allow = func(netip.AddrPort) bool { return true }
	f.LocalAddrs = noLocalAddrs

	_, err := f.Text(context.Background(), big.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")

	_, err = f.Text(context.Background(), "file:///etc/passwd")
	require.Error(t, err)
	_, err = f.Text(context.Background(), "ftp://example.com/x")
	require.Error(t, err)
}

func TestPageFetcher_ExtractsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer srv.Close()
	f := NewPageFetcher(5*time.Second, 1<<20)
	f.Allow = func(netip.AddrPort) bool { return true }
	f.LocalAddrs = noLocalAddrs
	text, err := f.Text(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Contains(t, text, "faster compiler")
}
