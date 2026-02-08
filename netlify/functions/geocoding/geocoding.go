package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"nawa-functions/internal"
	"net/http"
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
	})
	corsHeaders = map[string]string{
		"Access-Control-Allow-Origin":  "",
		"Access-Control-Allow-Headers": "X-Nawa-Token,x-nawa-token",
		"Access-Control-Allow-Methods": "*",
	}
	logger               = slog.New(slog.NewTextHandler(os.Stdout, nil))
	searchURL            = "https://api.mapbox.com/search/geocode/v6"
	forwardSearchURL     = searchURL + "/forward?country=us&access_token=" + os.Getenv("mapbox_access_token")
	reverseSearchURL     = searchURL + "/reverse?country=us&access_token=" + os.Getenv("mapbox_access_token")
	nawaToken            = os.Getenv("nawa_token")
	nawaKey              = os.Getenv("nawa_key")
	requireToken, _      = strconv.ParseBool(os.Getenv("require_token"))
	validatedClientToken = ""
)

const (
	localhostOrigin = "http://localhost:3000"
	githubOrigin    = "https://tshrestha.github.io"
)

func createResponse(req *events.APIGatewayProxyRequest, statusCode int, body string) *events.APIGatewayProxyResponse {
	origin := req.Headers["origin"]
	if origin == localhostOrigin || origin == githubOrigin {
		corsHeaders["Access-Control-Allow-Origin"] = origin
	}

	return &events.APIGatewayProxyResponse{
		StatusCode: statusCode,
		Body:       body,
		Headers:    corsHeaders,
	}
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
	req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
	req.Header.Set("Origin", "https://tshrestha.github.io")
	req.Header.Set("Referer", "https://tshrestha.github.io/nawa")

	res, err := httpClient.Do(req)
	if err != nil {
		logger.ErrorContext(ctx, "request failed", slog.String("reqURL", reqURL), slog.Any("error", err))
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusOK {
		body, err := io.ReadAll(res.Body)
		if err != nil {
			logger.ErrorContext(ctx, "failed to read response body", slog.String("reqURL", reqURL), slog.Any("error", err))
			return "", err
		}

		return string(body), nil
	}

	err = fmt.Errorf("received unexpected status code %d", res.StatusCode)
	logger.ErrorContext(ctx, "received unexpected status code", slog.String("reqURL", reqURL), slog.Int("statusCode", res.StatusCode))
	return "", err
}

func forwardSearch(ctx context.Context, req *events.APIGatewayProxyRequest, query string) *events.APIGatewayProxyResponse {
	cached := getCached(ctx, query)

	if cached == "" {
		reqURL := forwardSearchURL + "&q=" + query
		result, err := search(ctx, reqURL)
		if err != nil {
			return createResponse(req, http.StatusInternalServerError, err.Error())
		}

		setCache(ctx, query, result)
		return createResponse(req, http.StatusOK, result)
	}

	logger.InfoContext(ctx, "retrieved result from cache", slog.String("key", query))
	return createResponse(req, http.StatusOK, cached)
}

func reverseSearch(ctx context.Context, req *events.APIGatewayProxyRequest, lat, lon string) *events.APIGatewayProxyResponse {
	key := lat + lon
	cached := getCached(ctx, key)

	if cached == "" {
		reqURL := reverseSearchURL + "&latitude=" + lat + "&longitude=" + lon
		result, err := search(ctx, reqURL)
		if err != nil {
			return createResponse(req, http.StatusInternalServerError, err.Error())
		}

		setCache(ctx, key, result)
		return createResponse(req, http.StatusOK, result)
	}

	logger.InfoContext(ctx, "retrieved result from cache", slog.String("key", key))
	return createResponse(req, http.StatusOK, cached)
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	logger.InfoContext(ctx, "received request", slog.String("method", request.HTTPMethod), slog.String("path", request.Path))

	origin := request.Headers["origin"]
	if request.HTTPMethod == http.MethodOptions {
		logger.Info("received OPTIONS request", slog.String("origin", origin))
		return createResponse(&request, http.StatusOK, ""), nil
	}

	if request.HTTPMethod == http.MethodGet {
		if requireToken {
			logger.Info("client token is required")

			token := request.Headers["x-nawa-token"]
			if token == "" {
				return createResponse(&request, http.StatusUnauthorized, "invalid token"), nil
			}
			logger.Info("token header is present", slog.String("token", token))

			decrypted, err := internal.Decrypt(token, []byte(nawaKey))
			if err != nil {
				logger.Error("failed to decrypt token", slog.Any("error", err))
				return createResponse(&request, http.StatusInternalServerError, ""), nil
			}

			if string(decrypted) != nawaToken {
				logger.Error("decrypted client token does not match server token")
				return createResponse(&request, http.StatusUnauthorized, "invalid token"), nil
			}

			logger.Info("client token validated successfully")
		}

		pathSegments := strings.Split(request.Path, "/")
		requestType := pathSegments[len(pathSegments)-1]

		if requestType == "forward" {
			return forwardSearch(ctx, &request, request.QueryStringParameters["q"]), nil
		} else if requestType == "reverse" {
			return reverseSearch(ctx, &request, request.QueryStringParameters["lat"], request.QueryStringParameters["lon"]), nil
		}

		return createResponse(&request, http.StatusNotFound, ""), nil
	}

	return createResponse(&request, http.StatusMethodNotAllowed, ""), nil
}

func main() {
	lambda.Start(handler)
}
