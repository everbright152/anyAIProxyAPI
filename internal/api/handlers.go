package api

import (
	"encoding/json"
	"fmt"
	"github.com/chromedp/chromedp"
	"github.com/luispater/anyAIProxyAPI/internal/browser/chrome"
	"github.com/luispater/anyAIProxyAPI/internal/config"
	"github.com/luispater/anyAIProxyAPI/internal/html"
	"github.com/luispater/anyAIProxyAPI/internal/runner"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var (
	lastUsedPageIndex = make(map[string]int)
	pageIndexMutexes  = make(map[string]*sync.Mutex)
	mutexMapLock      = &sync.Mutex{}
	ScreenshotMutex   sync.Mutex
)

// APIHandlers contains the handlers for API endpoints
type APIHandlers struct {
	queue     *RequestQueue
	pages     map[string][]*chrome.Page
	debug     bool
	appConfig *config.AppConfig
}

// NewAPIHandlers creates a new API handlers instance
func NewAPIHandlers(appConfig *config.AppConfig, queue *RequestQueue, pages map[string][]*chrome.Page, debug bool) *APIHandlers {
	return &APIHandlers{
		queue:     queue,
		pages:     pages,
		debug:     debug,
		appConfig: appConfig,
	}
}

// validateAPIToken validates the API token for the given instance
func (h *APIHandlers) validateAPIToken(c *gin.Context, instanceName string) bool {
	// Get token from Authorization header
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		return false
	}

	// Extract token from "Bearer <token>" format
	var token string
	if strings.HasPrefix(authHeader, "Bearer ") {
		token = strings.TrimPrefix(authHeader, "Bearer ")
	} else {
		token = authHeader
	}

	if token == "" {
		return false
	}

	// Check global tokens first
	for _, globalToken := range h.appConfig.Tokens {
		if globalToken == token {
			return true
		}
	}

	// Check instance-specific tokens
	for _, instance := range h.appConfig.Instance {
		if instance.Name == instanceName {
			for _, instanceToken := range instance.Tokens {
				if instanceToken == token {
					return true
				}
			}
			break
		}
	}

	return false
}

// validateToken validates a token for the given instance
func (h *APIHandlers) validateToken(token string, instanceName string) bool {
	if token == "" {
		return false
	}

	// Check global tokens first
	for _, globalToken := range h.appConfig.Tokens {
		if globalToken == token {
			return true
		}
	}

	// Check instance-specific tokens
	for _, instance := range h.appConfig.Instance {
		if instance.Name == instanceName {
			for _, instanceToken := range instance.Tokens {
				if instanceToken == token {
					return true
				}
			}
			break
		}
	}

	return false
}

func (h *APIHandlers) TakeScreenshot(c *gin.Context) {
	defer ScreenshotMutex.Unlock()
	ScreenshotMutex.Lock()
	instanceName, ok := c.GetQuery("name")
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	instanceIndex, ok := c.GetQuery("index")
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	index, err := strconv.ParseUint(instanceIndex, 10, 64)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	if page, hasKey := h.pages[instanceName]; hasKey {
		var buf []byte
		pageCtx := page[index].GetContext()
		if err = chromedp.Run(pageCtx, chromedp.CaptureScreenshot(&buf)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to take screenshot: %v", err), "code": 500})
			return
		}
		c.Header("Content-Type", "image/png")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
		_, _ = c.Writer.Write(buf)
	} else {
		c.Status(http.StatusNotFound)
	}
}

func (h *APIHandlers) ProxyPac(c *gin.Context) {
	c.Status(http.StatusOK)
	c.Header("Content-Type", "application/x-ns-proxy-autoconfig")
	index, ok := c.GetQuery("index")
	if ok {
		i, err := strconv.ParseUint(index, 10, 32)
		if err != nil {
			_, _ = c.Writer.Write([]byte(`function FindProxyForURL(url, host) {return "PROXY 127.0.0.1:3120";}`))
			return
		}

		port := 3120 + i
		_, _ = c.Writer.Write([]byte(fmt.Sprintf(`function FindProxyForURL(url, host) {return "PROXY 127.0.0.1:%d";}`, port)))
	}
}

// ChatCompletions handles the /v1/chat/completions endpoint
func (h *APIHandlers) ChatCompletions(c *gin.Context) {
	rawJson, err := c.GetRawData()
	// If data retrieval fails, return 400 error
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid request: %v", err), "code": 400})
		return
	}

	instanceName := ""
	modelResult := gjson.GetBytes(rawJson, "model")
	if modelResult.Type == gjson.String {
		instanceName = strings.Split(modelResult.String(), "/")[0]
	}

	// Validate API token if tokens are configured
	hasGlobalTokens := len(h.appConfig.Tokens) > 0
	hasInstanceTokens := false
	for _, instance := range h.appConfig.Instance {
		if instance.Name == instanceName && len(instance.Tokens) > 0 {
			hasInstanceTokens = true
			break
		}
	}

	if hasGlobalTokens || hasInstanceTokens {
		if !h.validateAPIToken(c, instanceName) {
			c.JSON(http.StatusUnauthorized, ErrorResponse{
				Error: ErrorDetail{
					Message: "Invalid or missing API token",
					Type:    "authentication_error",
				},
			})
			return
		}
	}

	instanceIndex := 0
	for i := 0; i < len(h.appConfig.Instance); i++ {
		if h.appConfig.Instance[i].Name == instanceName {
			instanceIndex = i
		}
	}
	var page *chrome.Page
	defer func() {
		if page != nil {
			page.RequestMutex.Unlock()
		}
	}()

	if pages, ok := h.pages[instanceName]; ok {
		// Get the mutex for the instance
		mutex, exists := pageIndexMutexes[instanceName]
		if !exists {
			mutexMapLock.Lock()
			mutex = &sync.Mutex{}
			pageIndexMutexes[instanceName] = mutex
			mutexMapLock.Unlock()
		}

		// Lock the mutex to update the last used page index
		mutex.Lock()
		startIndex := lastUsedPageIndex[instanceName]
		currentIndex := (startIndex + 1) % len(pages)
		lastUsedPageIndex[instanceName] = currentIndex
		mutex.Unlock()

		// Reorder the pages to start from the last used index
		reorderedPages := make([]*chrome.Page, len(pages))
		for i := 0; i < len(pages); i++ {
			reorderedPages[i] = pages[(startIndex+1+i)%len(pages)]
		}

		locked := false
		for i := 0; i < len(reorderedPages); i++ {
			page = reorderedPages[i]
			if page.RequestMutex.TryLock() {
				locked = true
				break
			}
		}
		if !locked {
			page = pages[0]
			page.RequestMutex.Lock()
		}
	} else {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: ErrorDetail{
				Message: fmt.Sprintf("model \"%s\" not found.", modelResult),
				Type:    "not_found",
			},
		})
		return
	}

	// Generate unique task ID
	taskID := uuid.New().String()

	// Create a task
	task := &RequestTask{
		ID:           taskID,
		Request:      string(rawJson),
		Response:     make(chan *TaskResponse, 1),
		CreatedAt:    time.Now(),
		Context:      c,
		InstanceName: instanceName,
		Page:         page,
	}

	// Add a task to queue
	if err = h.queue.AddTask(task); err != nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{
			Error: ErrorDetail{
				Message: fmt.Sprintf("Failed to queue request: %v", err),
				Type:    "server_error",
			},
		})
		return
	}

	// Wait for response
	select {
	case response := <-task.Response:
		if !response.Success {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: ErrorDetail{
					Message: fmt.Sprintf("Processing failed: %v", response.Error),
					Type:    "server_error",
				},
			})
			return
		}

		streamResult := gjson.GetBytes(rawJson, "stream")
		if streamResult.Type == gjson.True {
			h.handleStreamingResponse(instanceIndex, c, response)
		} else {
			h.handleNonStreamingResponse(instanceIndex, c, response)
		}

	case <-time.After(5 * time.Minute): // 5 minute timeout
		c.JSON(http.StatusRequestTimeout, ErrorResponse{
			Error: ErrorDetail{
				Message: "Request timeout",
				Type:    "timeout_error",
			},
		})
		return
	}
}

func (h *APIHandlers) handleContextCanceled(instanceIndex int, page *chrome.Page) {
	r, err := runner.NewRunnerManager(h.appConfig.Instance[instanceIndex], page, h.debug, false)
	if err != nil {
		log.Error(err)
		return
	}

	r.SetVariable("PAGE", page, "ptr")
	err = r.Run("context_canceled")
	if err != nil {
		log.Error(err)
		return
	}
	log.Debugf("all of the rules are executed.")
}

// handleNonStreamingResponse handles non-streaming responses
func (h *APIHandlers) handleNonStreamingResponse(instanceIndex int, c *gin.Context, response *TaskResponse) {

	c.Header("Content-Type", "application/json")

	// Handle streaming manually
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	for {
		select {
		case <-c.Request.Context().Done():
			if c.Request.Context().Err().Error() == "context canceled" {
				log.Debugf("Client disconnected: %v", c.Request.Context().Err())
				h.handleContextCanceled(instanceIndex, response.Page)
				response.Runner.Abort()
			}
			return
		case chunk, okStream := <-response.Stream:
			if strings.HasPrefix(chunk, "{\"error\"") {
				c.Status(500)
				_, _ = fmt.Fprintf(c.Writer, chunk)
				flusher.Flush()
				return
			}

			if !okStream {
				return
			}

			c.Status(http.StatusOK)
			_, _ = fmt.Fprintf(c.Writer, "%s", chunk)
			flusher.Flush()
		case <-time.After(500 * time.Millisecond):
			// Write processing tag
			_, _ = c.Writer.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

// handleStreamingResponse handles streaming responses
func (h *APIHandlers) handleStreamingResponse(instanceIndex int, c *gin.Context, response *TaskResponse) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")

	// Handle streaming manually
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	for {
		select {
		case <-c.Request.Context().Done():
			if c.Request.Context().Err().Error() == "context canceled" {
				log.Debugf("Client disconnected: %v", c.Request.Context().Err())
				h.handleContextCanceled(instanceIndex, response.Page)
				response.Runner.Abort()
			}
			return
		case chunk, okStream := <-response.Stream:
			if strings.HasPrefix(chunk, "{\"error\"") {
				c.Status(500)
				_, _ = fmt.Fprintf(c.Writer, chunk)
				flusher.Flush()
				return
			}

			if !okStream {
				_, _ = fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}

			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", chunk)
			flusher.Flush()

		case <-time.After(300 * time.Second):
			_, _ = fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			flusher.Flush()
			return
		case <-time.After(500 * time.Millisecond):
			_, _ = c.Writer.Write([]byte(": ANY-AI-PROXY-API PROCESSING\n\n"))
			flusher.Flush()
		}
	}
}

// BrowserReload handles the /v1/browser/reload endpoint
func (h *APIHandlers) BrowserReload(c *gin.Context) {
	instanceName, ok := c.GetQuery("name")
	if !ok || instanceName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing required parameter: name", "code": 400})
		return
	}

	// Find the instance configuration
	var instanceConfig *config.AppConfigInstance
	for i := range h.appConfig.Instance {
		if h.appConfig.Instance[i].Name == instanceName {
			instanceConfig = &h.appConfig.Instance[i]
			break
		}
	}

	if instanceConfig == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Instance '%s' not found", instanceName), "code": 404})
		return
	}

	// Get the page for this instance
	page, exists := h.pages[instanceName]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Page for instance '%s' not found", instanceName), "code": 404})
		return
	}
	instanceIndex, ok := c.GetQuery("index")
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	index, err := strconv.ParseUint(instanceIndex, 10, 64)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	// Navigate to the instance URL
	pageCtx := page[index].GetContext()
	if err = chromedp.Run(pageCtx, chromedp.Navigate(instanceConfig.URL)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to reload page: %v", err), "code": 500})
		return
	}

	log.Debugf("Successfully reloaded page for instance '%s' to URL: %s", instanceName, instanceConfig.URL)
	c.JSON(http.StatusOK, gin.H{"message": "Page reloaded successfully"})
}

// AuthUpload handles the /v1/auth/upload endpoint
func (h *APIHandlers) AuthUpload(c *gin.Context) {
	var requestData struct {
		Name  string `json:"name" binding:"required"`
		Index *int   `json:"index" binding:"required"`
		Token string `json:"token" binding:"required"`
		Auth  string `json:"auth" binding:"required"`
	}

	if err := c.ShouldBindJSON(&requestData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid request data: %v", err), "code": 400})
		return
	}

	// Find the instance configuration
	var instanceConfig *config.AppConfigInstance
	for i := range h.appConfig.Instance {
		if h.appConfig.Instance[i].Name == requestData.Name {
			instanceConfig = &h.appConfig.Instance[i]
			break
		}
	}

	if instanceConfig == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Instance '%s' not found", requestData.Name), "code": 404})
		return
	}

	// Validate the token
	if !h.validateToken(requestData.Token, requestData.Name) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid verification token", "code": 401})
		return
	}

	// Validate the auth JSON format
	var authInfo chrome.AuthInfo
	if err := json.Unmarshal([]byte(requestData.Auth), &authInfo); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid auth JSON format: %v", err), "code": 400})
		return
	}

	// Ensure the auth directory exists
	authAbsPath, err := filepath.Abs(instanceConfig.Auth.Files[*requestData.Index])
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to get absolute path: %v", err), "code": 500})
		return
	}

	authDirName := filepath.Dir(authAbsPath)
	if _, err = os.Stat(authDirName); os.IsNotExist(err) {
		if err = os.MkdirAll(authDirName, 0755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to create auth directory: %v", err), "code": 500})
			return
		}
	}

	// Write the auth info to file
	if err = os.WriteFile(instanceConfig.Auth.Files[*requestData.Index], []byte(requestData.Auth), 0644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to write auth file: %v", err), "code": 500})
		return
	}

	// Get the page for this instance
	pages, exists := h.pages[instanceConfig.Name]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Page for instance '%s' not found", instanceConfig.Name), "code": 404})
		return
	}

	// Navigate to the instance URL
	pageCtx := pages[*requestData.Index].GetContext()
	err = chrome.ClearCookies(pageCtx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to load auth info: %v", err), "code": 500})
		return
	}
	pages[*requestData.Index].Close()

	pageLoaded := func() {
		r, errNewRunnerManager := runner.NewRunnerManager(*instanceConfig, pages[*requestData.Index], h.appConfig.Debug, false) // Pass pageCtx
		if errNewRunnerManager != nil {
			log.Error(errNewRunnerManager)
		}
		err = r.Run("init")
		if err != nil {
			log.Debug(err)
		}
		log.Debugf("all of the init system rules are executed.")
	}

	p, err := h.pages[instanceConfig.Name][*requestData.Index].GetBrowserManager().NewPage(instanceConfig.URL, instanceConfig.Adapter, instanceConfig.Auth.Files[*requestData.Index], pageLoaded)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to load auth info: %v", err), "code": 500})
		return
	}
	h.pages[instanceConfig.Name][*requestData.Index] = p
	pageCtx = p.GetContext()

	if err = chromedp.Run(pageCtx, chromedp.Navigate(instanceConfig.URL)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to reload page: %v", err), "code": 500})
		return
	}

	log.Debugf("Successfully uploaded auth info for instance '%s' to file: %s", requestData.Name, instanceConfig.Auth.Files[*requestData.Index])
	c.JSON(http.StatusOK, gin.H{"message": "Auth info uploaded successfully"})
}

// AuthUploadPage handles the GET /v1/auth/upload endpoint to serve the upload page
func (h *APIHandlers) AuthUploadPage(c *gin.Context) {
	// Serve the HTML content
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, string(html.AuthUploadHTML))
}

// AuthInstances handles the GET /v1/auth/instances endpoint to return available instances
func (h *APIHandlers) AuthInstances(c *gin.Context) {
	var instances []string
	for _, instance := range h.appConfig.Instance {
		instances = append(instances, instance.Name)
	}

	c.JSON(http.StatusOK, gin.H{
		"instances": instances,
	})
}
