package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

type Config struct {
	APIPrefix     string
	MaxRetryCount int
	RetryDelay    time.Duration
	FakeHeaders   map[string]string
}

var config Config

func init() {
	godotenv.Load()
	config = Config{
		APIPrefix:     getEnv("API_PREFIX", "/"),
		MaxRetryCount: getIntEnv("MAX_RETRY_COUNT", 3),
		RetryDelay:    getDurationEnv("RETRY_DELAY", 5000),
		FakeHeaders: map[string]string{
			"Accept":             "*/*",
			"Accept-Encoding":    "gzip, deflate, br, zstd",
			"Accept-Language":    "zh-CN,zh;q=0.9",
			"Origin":             "https://duckduckgo.com/",
			"Cookie":             "l=wt-wt; ah=wt-wt; dcm=6",
			"Dnt":                "1",
			"Priority":           "u=1, i",
			"Referer":            "https://duckduckgo.com/",
			"Sec-Ch-Ua":          `"Microsoft Edge";v="129", "Not(A:Brand";v="8", "Chromium";v="129"`,
			"Sec-Ch-Ua-Mobile":   "?0",
			"Sec-Ch-Ua-Platform": `"Windows"`,
			"Sec-Fetch-Dest":     "empty",
			"Sec-Fetch-Mode":     "cors",
			"Sec-Fetch-Site":     "same-origin",
			"User-Agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36",
		},
	}
}

func main() {
	r := gin.Default()
	r.Use(corsMiddleware())

	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "API 服务运行中~"})
	})

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	r.GET(config.APIPrefix+"/v1/models", func(c *gin.Context) {
		models := []gin.H{
			{"id": "gpt-4o-mini", "object": "model", "owned_by": "ddg"},
			{"id": "claude-3-haiku", "object": "model", "owned_by": "ddg"},
			{"id": "llama-3.1-70b", "object": "model", "owned_by": "ddg"},
			{"id": "mixtral-8x7b", "object": "model", "owned_by": "ddg"},
		}
		c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
	})

	r.POST(config.APIPrefix+"/v1/chat/completions", handleCompletion)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8787"
	}
	r.Run(":" + port)
}

func handleCompletion(c *gin.Context) {
	apiKey := os.Getenv("APIKEY")
	authorizationHeader := c.GetHeader("Authorization")

	if apiKey != "" {
		if authorizationHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "未提供 APIKEY"})
			return
		} else if !strings.HasPrefix(authorizationHeader, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "APIKEY 格式错误"})
			return
		} else {
			providedToken := strings.TrimPrefix(authorizationHeader, "Bearer ")
			if providedToken != apiKey {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "APIKEY无效"})
				return
			}
		}
	}

	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string      `json:"role"`
			Content interface{} `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	model := convertModel(req.Model)
	content := prepareMessages(req.Messages)
	// log.Printf("messages: %v", content)

	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": content,
			},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("请求体序列化失败: %v", err)})
		return
	}

	token, err := requestToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取token"})
		return
	}

	upstreamReq, err := http.NewRequest("POST", "https://duckduckgo.com/duckchat/v1/chat", strings.NewReader(string(body)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("创建请求失败: %v", err)})
		return
	}

	for k, v := range config.FakeHeaders {
		upstreamReq.Header.Set(k, v)
	}
	upstreamReq.Header.Set("x-vqd-4", token)
	upstreamReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(upstreamReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("请求失败: %v", err)})
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		// 启用 SSE 流式响应
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")

		flusher, ok := c.Writer.(http.Flusher)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
			return
		}

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					log.Printf("读取流式响应失败: %v", err)
				}
				break
			}

			if strings.HasPrefix(line, "data: ") {
				// 解析响应中的 JSON 数据块
				line = strings.TrimPrefix(line, "data: ")
				var chunk map[string]interface{}
				if err := json.Unmarshal([]byte(line), &chunk); err != nil {
					log.Printf("解析响应行失败: %v", err)
					continue
				}

				// 检查 chunk 是否包含 message
				if msg, exists := chunk["message"]; exists && msg != nil {
					if msgStr, ok := msg.(string); ok {
						response := map[string]interface{}{
							"id":      "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK",
							"object":  "chat.completion.chunk",
							"created": time.Now().Unix(),
							"model":   model,
							"choices": []map[string]interface{}{
								{
									"index": 0,
									"delta": map[string]string{
										"content": msgStr,
									},
									"finish_reason": nil,
								},
							},
						}
						// 将响应格式化为 SSE 数据块
						sseData, _ := json.Marshal(response)
						sseMessage := fmt.Sprintf("data: %s\n\n", sseData)

						// 发送数据并刷新缓冲区
						_, writeErr := c.Writer.Write([]byte(sseMessage))
						if writeErr != nil {
							log.Printf("写入响应失败: %v", writeErr)
							break
						}
						flusher.Flush()
					} else {
						log.Printf("chunk[message] 不是字符串: %v", msg)
					}
				} else {
					log.Println("chunk 中未包含 message 或 message 为 nil")
				}
			}
		}
	} else {
		// 非流式响应，返回完整的 JSON
		var fullResponse strings.Builder
		reader := bufio.NewReader(resp.Body)

		for {
			line, err := reader.ReadString('\n')
			if err == io.EOF {
				break
			} else if err != nil {
				log.Printf("读取响应失败: %v", err)
				break
			}

			if strings.HasPrefix(line, "data: ") {
				line = strings.TrimPrefix(line, "data: ")
				line = strings.TrimSpace(line)

				if line == "[DONE]" {
					break
				}

				var chunk map[string]interface{}
				if err := json.Unmarshal([]byte(line), &chunk); err != nil {
					log.Printf("解析响应行失败: %v", err)
					continue
				}

				if message, exists := chunk["message"]; exists {
					if msgStr, ok := message.(string); ok {
						fullResponse.WriteString(msgStr)
					}
				}
			}
		}

		// 返回完整 JSON 响应
		response := map[string]interface{}{
			"id":      "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"usage": map[string]int{
				"prompt_tokens":     0,
				"completion_tokens": 0,
				"total_tokens":      0,
			},
			"choices": []map[string]interface{}{
				{
					"message": map[string]string{
						"role":    "assistant",
						"content": fullResponse.String(),
					},
					"index": 0,
				},
			},
		}

		c.JSON(http.StatusOK, response)
	}
}

func requestToken() (string, error) {
	req, err := http.NewRequest("GET", "https://duckduckgo.com/duckchat/v1/status", nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %v", err)
	}
	for k, v := range config.FakeHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-vqd-accept", "1")

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	log.Println("发送 token 请求")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		log.Printf("requestToken: 非200响应: %d, 内容: %s\n", resp.StatusCode, bodyString)
		return "", fmt.Errorf("非200响应: %d, 内容: %s", resp.StatusCode, bodyString)
	}

	token := resp.Header.Get("x-vqd-4")
	if token == "" {
		return "", errors.New("响应中未包含x-vqd-4头")
	}

	// log.Printf("获取到的 token: %s\n", token)
	return token, nil
}

func prepareMessages(messages []struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}) string {
	var contentBuilder strings.Builder

	for _, msg := range messages {
		// Determine the role - 'system' becomes 'user'
		role := msg.Role
		if role == "system" {
			role = "user"
		}

		// Process the content as string
		contentStr := ""
		switch v := msg.Content.(type) {
		case string:
			contentStr = v
		case []interface{}:
			for _, item := range v {
				if itemMap, ok := item.(map[string]interface{}); ok {
					if text, exists := itemMap["text"].(string); exists {
						contentStr += text
					}
				}
			}
		default:
			contentStr = fmt.Sprintf("%v", msg.Content)
		}

		// Append the role and content to the builder
		contentBuilder.WriteString(fmt.Sprintf("%s:%s;\r\n", role, contentStr))
	}

	return contentBuilder.String()
}

func convertModel(inputModel string) string {
	switch strings.ToLower(inputModel) {
	case "claude-3-haiku":
		return "claude-3-haiku-20240307"
	case "llama-3.1-70b":
		return "meta-llama/Meta-Llama-3.1-70B-Instruct-Turbo"
	case "mixtral-8x7b":
		return "mistralai/Mixtral-8x7B-Instruct-v0.1"
	default:
		return "gpt-4o-mini"
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "*")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "*")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getIntEnv(key string, fallback int) int {
	if value, exists := os.LookupEnv(key); exists {
		var intValue int
		fmt.Sscanf(value, "%d", &intValue)
		return intValue
	}
	return fallback
}

func getDurationEnv(key string, fallback int) time.Duration {
	return time.Duration(getIntEnv(key, fallback)) * time.Millisecond
}
