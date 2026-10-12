package webhookcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func validateCustomerWebhookURL(ctx context.Context, rawURL string) error {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("webhook endpoint URL is invalid")
	}
	localProof := localWebhookProofEnabled()
	if parsed.Scheme != "https" && !(localProof && parsed.Scheme == "http") {
		return errors.New("webhook endpoint must use HTTPS")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil {
		return errors.New("webhook endpoint host could not be resolved")
	}
	if len(addresses) == 0 {
		return errors.New("webhook endpoint host resolved to no addresses")
	}
	if !localProof {
		for _, address := range addresses {
			if !publicWebhookAddress(address.IP) {
				return errors.New("webhook endpoint must resolve to public addresses")
			}
		}
	}
	return nil
}

func newCustomerWebhookHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	allowPrivate := localWebhookProofEnabled()
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid webhook network address")
			}
			portNumber, err := strconv.Atoi(port)
			if err != nil || portNumber < 1 || portNumber > 65535 {
				return nil, errors.New("invalid webhook port")
			}
			resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(resolved) == 0 {
				return nil, errors.New("webhook endpoint host could not be resolved")
			}
			for _, candidate := range resolved {
				if !allowPrivate && !publicWebhookAddress(candidate.IP) {
					return nil, errors.New("webhook endpoint must resolve to public addresses")
				}
			}
			dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
			var lastErr error
			for _, candidate := range resolved {
				connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
				if dialErr == nil {
					return connection, nil
				}
				lastErr = dialErr
			}
			return nil, fmt.Errorf("connect to webhook endpoint: %w", lastErr)
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func localWebhookProofEnabled() bool {
	environment := strings.ToLower(strings.TrimSpace(os.Getenv("OSAI_ENVIRONMENT")))
	if environment == "prod" || environment == "production" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OSAI_LOCAL_PROOF_MODE")), "true")
}

func publicWebhookAddress(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
	} {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
