package handler

import (
	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	"aurora/middlewares"

	"github.com/gin-gonic/gin"
)

func optionsHandler(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "POST")
	c.Header("Access-Control-Allow-Headers", "*")
	c.JSON(200, gin.H{
		"message": "pong",
	})
}

func RegisterRouter(accountPool *accounts.Pool, cfg *config.Config) *gin.Engine {
	chatHandler := NewChatHandler(accountPool, cfg)
	imageHandler := NewImageHandler(accountPool, cfg)
	audioHandler := NewAudioHandler(accountPool, cfg)
	authHandler := NewAuthHandler(accountPool)
	modelsHandler := NewModelsHandler()
	adminHandler := NewAdminHandler(accountPool, cfg)

	// 初始化基础前置参数（DPL、BasicCookies 等）
	proxyUrl := ""
	client := bogdanfinn.NewStdClient()
	chatgpt.GetDpl(client, proxyUrl)

	router := gin.Default()
	router.Use(middlewares.Cors)

	router.GET("/", func(c *gin.Context) { c.JSON(200, gin.H{"message": "Hello, world!"}) })
	router.GET("/ping", func(c *gin.Context) { c.JSON(200, gin.H{"message": "pong"}) })
	router.GET("/admin", adminHandler.Page)

	router.POST("/auth/session", authHandler.Session)
	router.POST("/auth/refresh", authHandler.Refresh)

	router.OPTIONS("/v1/chat/completions", optionsHandler)
	router.OPTIONS("/v1/models", optionsHandler)
	router.OPTIONS("/v1/responses", optionsHandler)
	router.OPTIONS("/v1/images/generations", optionsHandler)
	router.OPTIONS("/v1/images/edits", optionsHandler)
	router.OPTIONS("/v1/images/variations", optionsHandler)
	router.OPTIONS("/v1/files", optionsHandler)
	router.OPTIONS("/v1/audio/speech", optionsHandler)
	router.OPTIONS("/v1/audio/transcriptions", optionsHandler)
	router.OPTIONS("/v1/audio/translations", optionsHandler)

	authGroup := router.Group("").Use(middlewares.Authorization)
	authGroup.POST("/v1/chat/completions", chatHandler.Nightmare)
	authGroup.POST("/v1/responses", chatHandler.Responses)
	authGroup.POST("/v1/files", chatHandler.Files)
	authGroup.GET("/v1/models", modelsHandler.ListModels)
	authGroup.POST("/backend-api/conversation", chatHandler.ChatGPTConversation)
	authGroup.POST("/v1/images/generations", imageHandler.Generations)
	authGroup.POST("/v1/images/edits", imageHandler.Edits)
	authGroup.POST("/v1/images/variations", imageHandler.Variations)
	authGroup.POST("/v1/audio/speech", audioHandler.TTS)
	authGroup.POST("/v1/audio/transcriptions", audioHandler.Transcriptions)
	authGroup.POST("/v1/audio/translations", audioHandler.Translations)

	adminGroup := router.Group("/admin/api").Use(adminHandler.Authorize)
	adminGroup.GET("/accounts", adminHandler.ListAccounts)
	adminGroup.POST("/accounts", adminHandler.AddAccount)
	adminGroup.DELETE("/accounts/:source/:id", adminHandler.DeleteAccount)
	adminGroup.POST("/accounts/:source/:id/health", adminHandler.CheckAccountHealth)
	adminGroup.GET("/settings/api-key", adminHandler.GetAPIKey)
	adminGroup.PUT("/settings/api-key", adminHandler.UpdateAPIKey)

	return router
}
