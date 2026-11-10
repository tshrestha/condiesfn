package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"nawa-functions/internal"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/redis/go-redis/v9"
)

var (
	httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,              // Max total idle connections
			MaxIdleConnsPerHost: 20,               // Max idle connections per host
			IdleConnTimeout:     15 * time.Minute, // How long an idle connection stays open
		},
	}
	redisClient = redis.NewClient(&redis.Options{
		Addr:     os.Getenv("db_address"),
		Username: os.Getenv("db_username"),
		Password: os.Getenv("db_password"),
		DB:       0,
		// Lets rateLimitTimeout bound a Redis call that hangs.
		ContextTimeoutEnabled: true,
	})
	logger               = slog.New(slog.NewTextHandler(os.Stdout, nil))
	searchURL            = "https://api.mapbox.com/search/geocode/v6"
	mapboxAccessToken    = os.Getenv("mapbox_access_token")
	nawaToken            = os.Getenv("nawa_token")
	nawaKey              = os.Getenv("nawa_key")
	requireToken, _      = strconv.ParseBool(os.Getenv("require_token"))
	validatedClientToken = ""
)

const (
	localhostOrigin = "http://localhost:3000"
	githubOrigin    = "https://tshrestha.github.io"

	// The app debounces searches, so this leaves room for several people
	// sharing one address.
	rateLimitRequests = 60
	rateLimitWindow   = time.Minute
	rateLimitTimeout  = 2 * time.Second

	upstreamErrorMessage = "geocoding request failed"
)

// functionRequest is the event Netlify sends. rawQuery isn't part of the AWS
// event type.
type functionRequest struct {
	events.APIGatewayProxyRequest
	RawQuery string `json:"rawQuery"`
}

func isAllowedOrigin(origin string) bool {
	return origin == localhostOrigin || origin == githubOrigin
}

func createResponse(req *events.APIGatewayProxyRequest, statusCode int, body string) *events.APIGatewayProxyResponse {
	headers := map[string]string{"Vary": "Origin"}

	if origin := req.Headers["origin"]; isAllowedOrigin(origin) {
		headers["Access-Control-Allow-Origin"] = origin
		headers["Access-Control-Allow-Headers"] = "X-Nawa-Token,x-nawa-token"
		headers["Access-Control-Allow-Methods"] = "GET,OPTIONS"
	}

	return &events.APIGatewayProxyResponse{
		StatusCode: statusCode,
		Body:       body,
		Headers:    headers,
	}
}

// queryParams decodes the query string exactly once. rawQuery holds the string
// as the client sent it, so the result doesn't depend on how Netlify prepared
// queryStringParameters, which it doesn't document.
func queryParams(request *functionRequest) (url.Values, error) {
	if request.RawQuery != "" {
		return url.ParseQuery(request.RawQuery)
	}

	params := url.Values{}
	for name, value := range request.QueryStringParameters {
		params.Set(name, value)
	}
	return params, nil
}

// clientKey identifies the caller for rate limiting. Netlify's edge sets
// x-nf-client-connection-ip. X-Forwarded-For isn't used because callers can
// add to it.
func clientKey(req *events.APIGatewayProxyRequest) string {
	addr, err := netip.ParseAddr(req.Headers["x-nf-client-connection-ip"])
	if err != nil {
		logger.Warn("request has no usable client address", slog.Any("error", err))
		return "unknown"
	}

	addr = addr.Unmap()
	if addr.Is6() {
		// A single IPv6 customer usually controls a whole /64.
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}

// rateLimitResponse counts the request against the caller's fixed window and
// returns the response to send if the request must not proceed. The counter
// lives in Redis because function instances share no memory.
func rateLimitResponse(ctx context.Context, req *events.APIGatewayProxyRequest) *events.APIGatewayProxyResponse {
	ctx, cancel := context.WithTimeout(ctx, rateLimitTimeout)
	defer cancel()

	windowSeconds := int64(rateLimitWindow / time.Second)
	now := time.Now().Unix()
	key := fmt.Sprintf("ratelimit:geocoding:%s:%d", clientKey(req), now/windowSeconds)

	var count *redis.IntCmd
	_, err := redisClient.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		count = pipe.Incr(ctx, key)
		pipe.Expire(ctx, key, 2*rateLimitWindow)
		return nil
	})
	if err != nil {
		// Allowing the request here would let a flood that overwhelms Redis
		// reach Mapbox with no limit at all.
		logger.ErrorContext(ctx, "failed to check rate limit", slog.Any("error", err))
		return createResponse(req, http.StatusServiceUnavailable, "")
	}

	if count.Val() > rateLimitRequests {
		response := createResponse(req, http.StatusTooManyRequests, "")
		response.Headers["Retry-After"] = strconv.FormatInt(windowSeconds-now%windowSeconds, 10)
		return response
	}

	return nil
}

func mapboxURL(endpoint string, params url.Values) string {
	params.Set("country", "us")
	params.Set("access_token", mapboxAccessToken)

	// Encode writes a space as "+" and a literal "+" as "%2B", so this replaces
	// only spaces.
	return searchURL + endpoint + "?" + strings.ReplaceAll(params.Encode(), "+", "%20")
}

// withoutURL strips the request URL from err because it contains the Mapbox
// access token.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func getCached(ctx context.Context, key string) string {
	cached, err := redisClient.Get(ctx, key).Result()
	if err != nil {
		logger.WarnContext(ctx, "failed to retrieve query result from cache", slog.String("query", key), slog.Any("error", err))
		logger.InfoContext(ctx, "HTTP request is required to fetch query results", slog.String("query", key))
		return ""
	}

	return cached
}

func setCache(ctx context.Context, key, value string) {
	err := redisClient.Set(ctx, key, value, 200*time.Hour).Err()
	if err != nil {
		logger.ErrorContext(ctx, "failed to JSONSet forwardSearch result", slog.Any("error", err))
	}
}

func search(ctx context.Context, reqURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		err = withoutURL(err)
		logger.ErrorContext(ctx, "failed to create request", slog.Any("error", err))
		return "", err
	}
	req.Header.Set("Origin", "https://tshrestha.github.io")
	req.Header.Set("Referer", "https://tshrestha.github.io/nawa")

	res, err := httpClient.Do(req)
	if err != nil {
		err = withoutURL(err)
		logger.ErrorContext(ctx, "request failed", slog.String("path", req.URL.Path), slog.Any("error", err))
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusOK {
		body, err := io.ReadAll(res.Body)
		if err != nil {
			logger.ErrorContext(ctx, "failed to read response body", slog.String("path", req.URL.Path), slog.Any("error", err))
			return "", err
		}

		return string(body), nil
	}

	err = fmt.Errorf("received unexpected status code %d", res.StatusCode)
	logger.ErrorContext(ctx, "received unexpected status code", slog.String("path", req.URL.Path), slog.Int("statusCode", res.StatusCode))
	return "", err
}

// Cache keys are prefixed so a client-chosen query can't reach a rate-limit
// counter or a result cached for the other endpoint.
func forwardSearch(ctx context.Context, req *events.APIGatewayProxyRequest, query string) *events.APIGatewayProxyResponse {
	key := "forward:" + query
	cached := getCached(ctx, key)

	if cached == "" {
		reqURL := mapboxURL("/forward", url.Values{"q": {query}})
		result, err := search(ctx, reqURL)
		if err != nil {
			return createResponse(req, http.StatusInternalServerError, upstreamErrorMessage)
		}

		setCache(ctx, key, result)
		return createResponse(req, http.StatusOK, result)
	}

	logger.InfoContext(ctx, "retrieved result from cache", slog.String("key", key))
	return createResponse(req, http.StatusOK, cached)
}

func reverseSearch(ctx context.Context, req *events.APIGatewayProxyRequest, lat, lon string) *events.APIGatewayProxyResponse {
	key := "reverse:" + lat + "," + lon
	cached := getCached(ctx, key)

	if cached == "" {
		reqURL := mapboxURL("/reverse", url.Values{"latitude": {lat}, "longitude": {lon}})
		result, err := search(ctx, reqURL)
		if err != nil {
			return createResponse(req, http.StatusInternalServerError, upstreamErrorMessage)
		}

		setCache(ctx, key, result)
		return createResponse(req, http.StatusOK, result)
	}

	logger.InfoContext(ctx, "retrieved result from cache", slog.String("key", key))
	return createResponse(req, http.StatusOK, cached)
}

func handler(ctx context.Context, request functionRequest) (*events.APIGatewayProxyResponse, error) {
	req := &request.APIGatewayProxyRequest
	logger.InfoContext(ctx, "received request", slog.String("method", req.HTTPMethod), slog.String("path", req.Path))

	origin := req.Headers["origin"]
	if !isAllowedOrigin(origin) {
		logger.Warn("rejected request from disallowed origin", slog.String("origin", origin))
		return createResponse(req, http.StatusForbidden, ""), nil
	}

	if req.HTTPMethod == http.MethodOptions {
		logger.Info("received OPTIONS request", slog.String("origin", origin))
		return createResponse(req, http.StatusOK, ""), nil
	}

	if req.HTTPMethod == http.MethodGet {
		if response := rateLimitResponse(ctx, req); response != nil {
			return response, nil
		}

		if requireToken {
			logger.Info("client token is required")

			token := req.Headers["x-nawa-token"]
			if token == "" {
				return createResponse(req, http.StatusUnauthorized, "invalid token"), nil
			}
			logger.Info("token header is present", slog.String("token", token))

			decrypted, err := internal.Decrypt(token, []byte(nawaKey))
			if err != nil {
				logger.Error("failed to decrypt token", slog.Any("error", err))
				return createResponse(req, http.StatusInternalServerError, ""), nil
			}

			if string(decrypted) != nawaToken {
				logger.Error("decrypted client token does not match server token")
				return createResponse(req, http.StatusUnauthorized, "invalid token"), nil
			}

			logger.Info("client token validated successfully")
		}

		params, err := queryParams(&request)
		if err != nil {
			logger.Warn("failed to decode query string", slog.Any("error", err))
			return createResponse(req, http.StatusBadRequest, ""), nil
		}

		pathSegments := strings.Split(req.Path, "/")
		requestType := pathSegments[len(pathSegments)-1]

		if requestType == "forward" {
			return forwardSearch(ctx, req, params.Get("q")), nil
		} else if requestType == "reverse" {
			return reverseSearch(ctx, req, params.Get("lat"), params.Get("lon")), nil
		}

		return createResponse(req, http.StatusNotFound, ""), nil
	}

	return createResponse(req, http.StatusMethodNotAllowed, ""), nil
}

func main() {
	lambda.Start(handler)
}
