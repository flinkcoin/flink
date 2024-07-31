// Code generated by Wire. DO NOT EDIT.

//go:generate go run -mod=mod github.com/google/wire/cmd/wire
//go:build !wireinject
// +build !wireinject

package main

import (
	"libs/core"
	"log/slog"
)

// Injectors from wire.go:

func NewLogger() *slog.Logger {
	slogLogger := core.CreateLogger()
	return slogLogger
}

func NewConfig() *Config {
	mainConfig := CreateConfig()
	return mainConfig
}
