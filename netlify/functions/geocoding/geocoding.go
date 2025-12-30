package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/paulmach/orb/geojson"
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
		"Access-Control-Allow-Headers": "*",
		"Access-Control-Allow-Methods": "*",
	}
	logger             = slog.New(slog.NewTextHandler(os.Stdout, nil))
	mapboxGeocodingURL = "https://api.mapbox.com/search/geocode/v6/forward?country=us&types=place&access_token=" + os.Getenv("mapbox_access_token")
)

const (
	localhostOrigin = "http://localhost:3000"
	githubOrigin    = "https://tshrestha.github.io"
)

func geocoding(ctx context.Context, query string) (string, error) {
	reqURL := mapboxGeocodingURL + "&q=" + query
	req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
	req.Header.Set("Origin", "https://tshrestha.github.io")
	req.Header.Set("Referer", "https://tshrestha.github.io/nawa")

	res, err := httpClient.Do(req)
	if err != nil {
		logger.ErrorContext(ctx, "geocoding request failed", slog.Any("error", err))
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusOK {
		body, err := io.ReadAll(res.Body)
		if err != nil {
			logger.ErrorContext(ctx, "failed to read response body", slog.Any("error", err))
			return "", err
		}

		var featureCollection geojson.FeatureCollection
		err = json.Unmarshal(body, &featureCollection)
		if err != nil {
			logger.ErrorContext(ctx, "failed to unmarshall geocoding result", slog.Any("error", err))
		}

		err = redisClient.JSONSet(ctx, query, "$", featureCollection).Err()
		if err != nil {
			logger.ErrorContext(ctx, "failed to JSONSet geocoding result", slog.Any("error", err))
		}

		return string(body), nil
	}

	err = fmt.Errorf("received unexpected status code %d", res.StatusCode)
	logger.ErrorContext(ctx, "received unexpected status code", slog.Int("statusCode", res.StatusCode))
	return "", err
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	logger.InfoContext(ctx, "received request", slog.String("method", request.HTTPMethod), slog.String("path", request.Path))

	origin := request.Headers["Origin"]
	if request.HTTPMethod == http.MethodOptions && origin == localhostOrigin || origin == githubOrigin {
		corsHeaders["Access-Control-Allow-Origin"] = origin

		return &events.APIGatewayProxyResponse{
			StatusCode: http.StatusOK,
			Headers:    corsHeaders,
		}, nil
	}

	if request.HTTPMethod == http.MethodGet {
		query := request.QueryStringParameters["q"]
		cached, err := redisClient.JSONGet(ctx, query, "$").Result()

		if err != nil {
			logger.WarnContext(ctx, "failed to retrieve query result from cache", slog.String("query", query), slog.Any("error", err))
			logger.InfoContext(ctx, "HTTP request is required to fetch query results", slog.String("query", query))

			result, err := geocoding(ctx, query)
			if err != nil {
				return &events.APIGatewayProxyResponse{
					StatusCode: http.StatusInternalServerError,
					Body:       err.Error(),
				}, nil
			}

			return &events.APIGatewayProxyResponse{
				StatusCode: http.StatusOK,
				Body:       result,
			}, nil
		}

		logger.InfoContext(ctx, "retrieve query result from cache", slog.String("query", query))
		return &events.APIGatewayProxyResponse{
			StatusCode: http.StatusOK,
			Body:       cached,
		}, nil
	}

	return &events.APIGatewayProxyResponse{
		StatusCode: http.StatusMethodNotAllowed,
	}, nil
}

func main() {
	lambda.Start(handler)
}
