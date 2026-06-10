package axis_airquality

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// digestTransport is an http.RoundTripper that transparently performs HTTP
// Digest authentication (RFC 2617, qop="auth", MD5). Axis devices require
// digest authentication by default.
type digestTransport struct {
	username string
	password string
	base     http.RoundTripper

	mu        sync.Mutex
	challenge *digestChallenge
	nc        uint64
}

type digestChallenge struct {
	realm     string
	nonce     string
	opaque    string
	algorithm string
	qop       string
}

func newDigestTransport(username, password string, insecureSkipVerify bool) *digestTransport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	return &digestTransport{
		username: username,
		password: password,
		base:     transport,
	}
}

func (d *digestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Capture the body so the request can be replayed after a 401 challenge.
	body, err := drainBody(req)
	if err != nil {
		return nil, err
	}

	if challenge := d.currentChallenge(); challenge != nil {
		if err := d.setAuthHeader(req, challenge); err != nil {
			return nil, err
		}
	}

	resp, err := d.base.RoundTrip(cloneWithBody(req, body))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if challenge == nil {
		return resp, nil
	}
	// Drain and close the unauthorized response before retrying.
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	d.storeChallenge(challenge)

	retry := cloneWithBody(req, body)
	if err := d.setAuthHeader(retry, challenge); err != nil {
		return nil, err
	}
	return d.base.RoundTrip(retry)
}

func (d *digestTransport) currentChallenge() *digestChallenge {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.challenge
}

func (d *digestTransport) storeChallenge(c *digestChallenge) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.challenge = c
}

func (d *digestTransport) setAuthHeader(req *http.Request, c *digestChallenge) error {
	d.mu.Lock()
	d.nc++
	nc := d.nc
	d.mu.Unlock()

	header, err := buildAuthHeader(d.username, d.password, req.Method, req.URL.RequestURI(), c, nc)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", header)
	return nil
}

func buildAuthHeader(username, password, method, uri string, c *digestChallenge, nc uint64) (string, error) {
	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", username, c.realm, password))
	ha2 := md5Hex(fmt.Sprintf("%s:%s", method, uri))

	cnonce, err := randomCnonce()
	if err != nil {
		return "", err
	}
	ncValue := fmt.Sprintf("%08x", nc)

	var response string
	qop := selectQop(c.qop)
	if qop != "" {
		response = md5Hex(fmt.Sprintf("%s:%s:%s:%s:%s:%s", ha1, c.nonce, ncValue, cnonce, qop, ha2))
	} else {
		response = md5Hex(fmt.Sprintf("%s:%s:%s", ha1, c.nonce, ha2))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username=%q, realm=%q, nonce=%q, uri=%q, response=%q`,
		username, c.realm, c.nonce, uri, response)
	if c.algorithm != "" {
		fmt.Fprintf(&b, `, algorithm=%s`, c.algorithm)
	}
	if qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce=%q`, qop, ncValue, cnonce)
	}
	if c.opaque != "" {
		fmt.Fprintf(&b, `, opaque=%q`, c.opaque)
	}
	return b.String(), nil
}

func selectQop(qop string) string {
	for _, q := range strings.Split(qop, ",") {
		if strings.TrimSpace(q) == "auth" {
			return "auth"
		}
	}
	return ""
}

func parseChallenge(header string) *digestChallenge {
	if header == "" {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(header), "digest ") {
		return nil
	}
	parts := splitParams(header[len("Digest "):])
	c := &digestChallenge{
		realm:     parts["realm"],
		nonce:     parts["nonce"],
		opaque:    parts["opaque"],
		algorithm: parts["algorithm"],
		qop:       parts["qop"],
	}
	if c.nonce == "" {
		return nil
	}
	return c
}

// splitParams parses a comma-separated list of key=value digest parameters,
// honoring quoted values that may contain commas.
func splitParams(s string) map[string]string {
	result := make(map[string]string)
	var key, value strings.Builder
	inKey := true
	inQuotes := false

	flush := func() {
		k := strings.TrimSpace(key.String())
		v := strings.Trim(strings.TrimSpace(value.String()), `"`)
		if k != "" {
			result[strings.ToLower(k)] = v
		}
		key.Reset()
		value.Reset()
		inKey = true
	}

	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == '=' && inKey && !inQuotes:
			inKey = false
		case r == ',' && !inQuotes:
			flush()
		case inKey:
			key.WriteRune(r)
		default:
			value.WriteRune(r)
		}
	}
	flush()
	return result
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomCnonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	return body, nil
}

func cloneWithBody(req *http.Request, body []byte) *http.Request {
	clone := req.Clone(req.Context())
	if body != nil {
		clone.Body = io.NopCloser(strings.NewReader(string(body)))
		clone.ContentLength = int64(len(body))
	}
	return clone
}
