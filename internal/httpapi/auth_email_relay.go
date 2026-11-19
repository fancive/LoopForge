package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"
)

var signupRelayClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// signupRelayThroughSocket tunnels only this client's HTTPS connection.
// The URL still supplies the TLS server name and certificate verification.
func signupRelayThroughSocket(socket string) (*http.Client, func(), error) {
	if !filepath.IsAbs(socket) {
		return nil, nil, errors.New("signup mail relay socket must be an absolute path")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	client := *signupRelayClient
	client.Transport = transport
	return &client, transport.CloseIdleConnections, nil
}

func sendSignupCodeByRelay(ctx context.Context, client *http.Client, endpoint, token, email, code string) error {
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || token == "" {
		return errors.New("invalid signup mail relay configuration")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"channel": "email", "mailFrom": token, "receiver": email,
		"mailSubject": signupEmailSubject, "mailHtml": "false", "content": signupEmailBody(code),
	} {
		if err := form.WriteField(key, value); err != nil {
			return err
		}
	}
	if err := form.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result struct {
		Code    *int  `json:"code"`
		Success *bool `json:"success"`
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 ||
		json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&result) != nil {
		return errors.New("invalid signup mail relay response")
	}
	if result.Code != nil {
		// A business error code takes precedence over the success flag:
		// some relay versions describe successful HTTP handling with that flag.
		if (*result.Code == 0 || *result.Code == http.StatusOK) && (result.Success == nil || *result.Success) {
			return nil
		}
	} else if result.Success != nil && *result.Success {
		return nil
	}
	return errors.New("signup mail relay rejected the message")
}
