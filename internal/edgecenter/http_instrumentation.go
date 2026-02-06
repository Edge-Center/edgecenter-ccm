package edgecenter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	reUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reHex    = regexp.MustCompile(`(?i)\b[0-9a-f]{16,}\b`)
	reNumber = regexp.MustCompile(`\b\d+\b`)
)

type instrumentedRoundTripper struct {
	next http.RoundTripper
}

func (rt *instrumentedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	method := req.Method
	path := sanitizePath(req.URL.Path)

	resp, err := rt.next.RoundTrip(req)

	if err != nil {
		reason := classifyTransportError(err)

		apiRequestErrorsTotal.
			WithLabelValues(method, path, reason).
			Inc()

		apiRequestsTotal.
			WithLabelValues(method, path, "error").
			Inc()

		apiRequestDuration.
			WithLabelValues(method, path, "error").
			Observe(time.Since(start).Seconds())

		return nil, err
	}

	code := strconv.Itoa(resp.StatusCode)

	apiRequestsTotal.
		WithLabelValues(method, path, code).
		Inc()

	apiRequestDuration.
		WithLabelValues(method, path, code).
		Observe(time.Since(start).Seconds())

	return resp, nil
}

func classifyTransportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network_error"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}

	return "unknown"
}

func sanitizePath(path string) string {
	p := path

	p = reUUID.ReplaceAllString(p, ":id")
	p = reHex.ReplaceAllString(p, ":id")
	p = reNumber.ReplaceAllString(p, ":id")

	return limitPathDepth(p, 6)
}

func limitPathDepth(path string, maxSegments int) string {
	if maxSegments <= 0 {
		return "/"
	}

	parts := strings.Split(path, "/")

	out := make([]string, 0, maxSegments)

	for _, part := range parts {
		if part == "" {
			continue
		}

		out = append(out, part)

		if len(out) >= maxSegments {
			break
		}
	}

	if len(out) == 0 {
		return "/"
	}

	return "/" + strings.Join(out, "/")
}
