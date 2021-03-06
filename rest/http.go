package rest

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/pkg/errors"
	"github.com/spf13/viper"

	"github.com/akitasoftware/akita-cli/cfg"
	"github.com/akitasoftware/akita-cli/printer"
	"github.com/akitasoftware/akita-cli/version"
)

const (
	// TODO: Make this tunable.
	defaultClientTimeout = 5 * time.Second
)

var (
	// Shared client to maximize connection re-use.
	// TODO: make this private to the package once kgx package is removed.
	HTTPClient *retryablehttp.Client
)

// Error type for non-2xx HTTP errors.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

func (he HTTPError) Error() string {
	if he.StatusCode == 401 {
		return `Invalid credentials, run "login" or use AKITA_API_KEY_SECRET environment variable`
	}
	return fmt.Sprintf("received status code %d, body: %s", he.StatusCode, string(he.Body))
}

// Implements retryablehttp LeveledLogger interface using printer.
type printerLogger struct{}

func (printerLogger) Error(f string, args ...interface{}) {
	printer.Errorln(f, args)
}

func (printerLogger) Info(f string, args ...interface{}) {
	printer.Infoln(f, args)
}

func (printerLogger) Debug(f string, args ...interface{}) {
	// Use verbose logging so users don't see every interaction with Akita API by
	// default they enable --debug.
	printer.V(4).Debugln(f, args)
}

func (printerLogger) Warn(f string, args ...interface{}) {
	printer.Warningln(f, args)
}

func init() {
	HTTPClient = retryablehttp.NewClient()

	transport := &http.Transport{
		MaxIdleConns:    3,
		IdleConnTimeout: 60 * time.Second,
	}
	HTTPClient.HTTPClient = &http.Client{
		Transport: transport,
	}

	HTTPClient.RetryWaitMin = 100 * time.Millisecond
	HTTPClient.RetryWaitMax = 1 * time.Second
	HTTPClient.RetryMax = 3
	HTTPClient.Logger = printerLogger{}
	HTTPClient.ErrorHandler = retryablehttp.PassthroughErrorHandler
}

func sendRequest(ctx context.Context, req *http.Request) ([]byte, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		c, cancel := context.WithTimeout(ctx, defaultClientTimeout)
		defer cancel()
		ctx = c
	}

	apiKeyID, apiKeySecret := cfg.GetAPIKeyAndSecret()
	if apiKeyID == "" {
		return nil, errors.New(`API key ID not found, run "login" or use AKITA_API_KEY_ID environment variable`)
	}
	if apiKeySecret == "" {
		return nil, errors.New(`API key secret not found, run "login" or use AKITA_API_KEY_SECRET environment variable`)
	}
	req.SetBasicAuth(apiKeyID, apiKeySecret)

	req.Header.Set("user-agent", GetUserAgent())

	// Inlcude the git SHA that this copy of the CLI was built from. Its purpose
	// is two-fold:
	// 1. The presence of this header is used as a heuristic to identify witnesses
	// 		that contain akita's API traffic rather than actual user traffic.
	// 2. As extra debug info, since the CLI semantic version is only incremented
	// 		on release, so there could be many experimental builds from different
	//		git commits with the same semantic version.
	req.Header.Set("x-akita-cli-git-version", version.GitVersion())

	if viper.GetBool("dogfood") {
		req.Header.Set("x-akita-dogfood", "true")
	}

	retryableReq, err := retryablehttp.FromRequest(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to convert HTTP request into retryable request")
	}
	resp, err := HTTPClient.Do(retryableReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if respBody, err := ioutil.ReadAll(resp.Body); err != nil {
		return nil, errors.Wrap(err, "failed to read response body")
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, HTTPError{StatusCode: resp.StatusCode, Body: respBody}
	} else {
		return respBody, nil
	}
}
