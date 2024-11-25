package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	APIEndpoint string
	Port        string
	SSLCert     string
	SSLKey      string
	EnableHTTPS bool
}

type RadioStation struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
}

type StationResponse struct {
	Name string `json:"name"`
}

// Prometheus metrics
var (
	stationRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "radio_station_requests_total",
			Help: "The total number of requests per station",
		},
		[]string{"station"},
	)

	streamErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "radio_stream_errors_total",
			Help: "The total number of streaming errors",
		},
	)

	activeStreams = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "radio_active_streams",
			Help: "The number of currently active streams",
		},
	)
)

func parseConfig() Config {
	var config Config

	flag.StringVar(&config.APIEndpoint, "api", "", "Radio stations API endpoint")
	flag.StringVar(&config.Port, "port", "8080", "Port to listen on")
	flag.StringVar(&config.SSLCert, "cert", "", "Path to SSL certificate file")
	flag.StringVar(&config.SSLKey, "key", "", "Path to SSL private key file")

	flag.Parse()

	// Environment variable overrides
	if apiEnv := os.Getenv("RADIO_API_ENDPOINT"); apiEnv != "" {
		config.APIEndpoint = apiEnv
	}
	if portEnv := os.Getenv("RADIO_PORT"); portEnv != "" {
		config.Port = portEnv
	}

	if config.APIEndpoint == "" {
		log.Fatal("Error: API endpoint must be provided")
	}

	config.EnableHTTPS = config.SSLCert != "" && config.SSLKey != ""

	return config
}

func main() {
	config := parseConfig()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	logger := log.New(os.Stdout, "[Radio-API] ", log.LstdFlags)

	r.GET("/stations", getStationsHandler(config, logger))
	r.GET("/stream/:station", streamStationHandler(config, logger))
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy"})
	})

	serverAddr := fmt.Sprintf(":%s", config.Port)
	logger.Printf("Starting server on %s", serverAddr)

	if config.EnableHTTPS {
		logger.Printf("Starting HTTPS server on port %s...", config.Port)
		logger.Fatal(r.RunTLS(serverAddr, config.SSLCert, config.SSLKey))
	} else {
		logger.Printf("Starting HTTP server on port %s...", config.Port)
		logger.Fatal(r.Run(serverAddr))
	}
}

func getStationsHandler(config Config, logger *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := http.Get(config.APIEndpoint)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch stations"})
			return
		}
		defer resp.Body.Close()

		var stations []RadioStation
		if err := json.NewDecoder(resp.Body).Decode(&stations); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse stations"})
			return
		}

		var response []StationResponse
		for _, station := range stations {
			response = append(response, StationResponse{Name: station.Name})
		}

		c.JSON(http.StatusOK, response)
	}
}

func streamStationHandler(config Config, logger *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		stationName := c.Param("station")
		stationRequests.WithLabelValues(stationName).Inc()

		// Fetch stations to get URL
		resp, err := http.Get(config.APIEndpoint)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch stations"})
			return
		}
		defer resp.Body.Close()

		var stations []RadioStation
		if err := json.NewDecoder(resp.Body).Decode(&stations); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse stations"})
			return
		}

		// Find station URL
		var targetStation RadioStation
		found := false
		for _, station := range stations {
			if strings.EqualFold(station.Name, stationName) {
				targetStation = station
				found = true
				break
			}
		}

		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "Station not found"})
			return
		}

		// Create request to stream
		req, err := http.NewRequest("GET", targetStation.URL, nil)
		if err != nil {
			streamErrors.Inc()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create stream request"})
			return
		}

		// Set ICY/Shoutcast headers
		req.Header.Set("Icy-MetaData", "1")
		req.Header.Set("User-Agent", "ICY/5.0")

		// Execute request
		streamResp, err := http.DefaultClient.Do(req)
		if err != nil {
			streamErrors.Inc()
			logger.Printf("Stream connection error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to connect to stream"})
			return
		}
		defer streamResp.Body.Close()

		// Log ICY headers for debugging
		logICYHeaders(logger, streamResp)

		// Set appropriate headers
		c.Header("Content-Type", getContentType(streamResp))
		c.Header("Transfer-Encoding", "chunked")

		// Track active streams
		activeStreams.Inc()
		defer activeStreams.Dec()

		// Stream with context cancellation support
		done := make(chan struct{})
		errChan := make(chan error, 1)

		go func() {
			defer close(done)

			// Use buffered writer for efficiency
			buffWriter := bufio.NewWriterSize(c.Writer, 32*1024)

			// Stream with buffer
			_, err := io.Copy(buffWriter, streamResp.Body)
			if err != nil {
				errChan <- err
				return
			}

			// Flush any remaining data
			buffWriter.Flush()
		}()

		// Wait for stream completion or context cancellation
		select {
		case err := <-errChan:
			logger.Printf("Stream error: %v", err)
			streamErrors.Inc()
			c.AbortWithStatus(http.StatusInternalServerError)
		case <-c.Done():
			logger.Println("Stream cancelled by client")
		case <-done:
			logger.Println("Stream completed")
		}
	}
}

// Helper to log ICY/Shoutcast headers
func logICYHeaders(logger *log.Logger, resp *http.Response) {
	headers := []string{
		"icy-pub", "icy-description", "icy-url",
		"icy-name", "icy-genre", "icy-br",
		"icy-metaint", "Content-Type",
	}

	logger.Println("Stream Headers:")
	for _, header := range headers {
		if val := resp.Header.Get(header); val != "" {
			logger.Printf("%s: %s", header, val)
		}
	}
}

// Determine content type, with fallback
func getContentType(resp *http.Response) string {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		// Common streaming content types
		contentTypes := []string{
			"audio/aac",
			"audio/mpeg",
			"audio/ogg",
			"audio/wav",
		}
		for _, ct := range contentTypes {
			if strings.Contains(resp.Header.Get("icy-br"), ct) {
				return ct
			}
		}
		return "application/octet-stream"
	}
	return contentType
}
