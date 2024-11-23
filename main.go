package main

import (
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "strings"
    "time"

    "github.com/gin-gonic/gin"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
    APIEndpoint  string
    Port         string
    SSLCert     string
    SSLKey      string
    EnableHTTPS bool
}

type RadioStation struct {
    ID        int       `json:"id"`
    CreatedAt time.Time `json:"created_at"`
    Name      string    `json:"name"`
    URL       string    `json:"url"`
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
    
    apiLatency = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "radio_api_latency_seconds",
            Help:    "The latency of API requests",
            Buckets: prometheus.DefBuckets,
        },
        []string{"endpoint"},
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

func getEnv(key, fallback string) string {
    if value, exists := os.LookupEnv(key); exists {
        return value
    }
    return fallback
}

func parseConfig() Config {
    var config Config
    
    // Command line flags
    flag.StringVar(&config.APIEndpoint, "api", "", "Radio stations API endpoint")
    flag.StringVar(&config.Port, "port", "", "Port to listen on")
    flag.StringVar(&config.SSLCert, "cert", "", "Path to SSL certificate file")
    flag.StringVar(&config.SSLKey, "key", "", "Path to SSL private key file")
    
    flag.Parse()
    
    // Environment variables override flags
    config.APIEndpoint = getEnv("RADIO_API_ENDPOINT", config.APIEndpoint)
    config.Port = getEnv("RADIO_PORT", config.Port)
    config.SSLCert = getEnv("RADIO_SSL_CERT", config.SSLCert)
    config.SSLKey = getEnv("RADIO_SSL_KEY", config.SSLKey)
    
    // Set defaults if not provided
    if config.Port == "" {
        config.Port = "8080"
    }
    
    if config.APIEndpoint == "" {
        log.Fatal("Error: API endpoint must be provided via -api flag or RADIO_API_ENDPOINT environment variable")
    }
    
    config.EnableHTTPS = config.SSLCert != "" && config.SSLKey != ""
    if config.EnableHTTPS && (config.SSLCert == "" || config.SSLKey == "") {
        log.Fatal("Error: both certificate and key are required for HTTPS")
    }
    
    return config
}

func main() {
    config := parseConfig()
    
    // Set Gin to release mode in production
    gin.SetMode(gin.ReleaseMode)
    
    r := gin.Default()
    r.Use(corsMiddleware())
    
    // Create a new logger instance
    logger := log.New(log.Writer(), "[Radio-API] ", log.LstdFlags)
    
    // Routes
    r.GET("/stations", getStationsHandler(config, logger))
    r.GET("/stream/:station", streamStationHandler(config, logger))
    r.GET("/health", healthCheckHandler())
    
    // Prometheus metrics endpoint
    r.GET("/metrics", gin.WrapH(promhttp.Handler()))
    
    serverAddr := fmt.Sprintf(":%s", config.Port)
    
    if config.EnableHTTPS {
        logger.Printf("Starting HTTPS server on port %s...", config.Port)
        logger.Fatal(r.RunTLS(serverAddr, config.SSLCert, config.SSLKey))
    } else {
        logger.Printf("Starting HTTP server on port %s...", config.Port)
        logger.Fatal(r.Run(serverAddr))
    }
}

func corsMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
        c.Writer.Header().Set("Access-Control-Allow-Methods", "GET")
        c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
        
        if c.Request.Method == "OPTIONS" {
            c.AbortWithStatus(204)
            return
        }
        
        c.Next()
    }
}

func healthCheckHandler() gin.HandlerFunc {
    return func(c *gin.Context) {
        c.JSON(http.StatusOK, gin.H{
            "status": "healthy",
            "time":   time.Now().Format(time.RFC3339),
        })
    }
}

func getStationsHandler(config Config, logger *log.Logger) gin.HandlerFunc {
    return func(c *gin.Context) {
        timer := prometheus.NewTimer(apiLatency.WithLabelValues("/stations"))
        defer timer.ObserveDuration()
        
        resp, err := http.Get(config.APIEndpoint)
        if err != nil {
            logger.Printf("Error fetching stations: %v", err)
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch stations"})
            return
        }
        defer resp.Body.Close()
        
        var stations []RadioStation
        if err := json.NewDecoder(resp.Body).Decode(&stations); err != nil {
            logger.Printf("Error parsing stations: %v", err)
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse stations"})
            return
        }
        
        var response []StationResponse
        for _, station := range stations {
            response = append(response, StationResponse{
                Name: station.Name,
            })
        }
        
        logger.Printf("Successfully returned %d stations", len(response))
        c.JSON(http.StatusOK, response)
    }
}

func streamStationHandler(config Config, logger *log.Logger) gin.HandlerFunc {
    return func(c *gin.Context) {
        stationName := c.Param("station")
        logger.Printf("Streaming request for station: %s", stationName)
        
        // Increment request counter for this station
        stationRequests.WithLabelValues(stationName).Inc()
        
        timer := prometheus.NewTimer(apiLatency.WithLabelValues("/stream"))
        defer timer.ObserveDuration()
        
        resp, err := http.Get(config.APIEndpoint)
        if err != nil {
            logger.Printf("Error fetching stations: %v", err)
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch stations"})
            return
        }
        defer resp.Body.Close()
        
        var stations []RadioStation
        if err := json.NewDecoder(resp.Body).Decode(&stations); err != nil {
            logger.Printf("Error parsing stations: %v", err)
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse stations"})
            return
        }
        
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
            logger.Printf("Station not found: %s", stationName)
            c.JSON(http.StatusNotFound, gin.H{"error": "Station not found"})
            return
        }
        
        streamResp, err := http.Get(targetStation.URL)
        if err != nil {
            streamErrors.Inc()
            logger.Printf("Error connecting to radio stream: %v", err)
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to connect to radio stream"})
            return
        }
        defer streamResp.Body.Close()
        
        c.Header("Content-Type", streamResp.Header.Get("Content-Type"))
        c.Header("Transfer-Encoding", "chunked")
        
        activeStreams.Inc()
        defer activeStreams.Dec()
        
        _, err = io.Copy(c.Writer, streamResp.Body)
        if err != nil {
            streamErrors.Inc()
            logger.Printf("Streaming error: %v", err)
        }
    }
}
