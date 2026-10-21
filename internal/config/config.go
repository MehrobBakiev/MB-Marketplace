package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	Address        string
	DatabaseURL    string
	JWTSecret      string
	AdminEmail     string
	AdminPassword  string
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	CreateInterval time.Duration
	UpdateInterval time.Duration
}

func Load() (Config, error) {
	c := Config{Address: env("HTTP_ADDR", ":8080"), DatabaseURL: os.Getenv("DATABASE_URL"), JWTSecret: os.Getenv("JWT_SECRET"), AdminEmail: os.Getenv("ADMIN_EMAIL"), AdminPassword: os.Getenv("ADMIN_PASSWORD")}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("JWT_SECRET must contain at least 32 bytes")
	}
	for _, field := range []struct {
		name, fallback string
		target         *time.Duration
	}{
		{"ACCESS_TOKEN_TTL", "20m", &c.AccessTTL}, {"REFRESH_TOKEN_TTL", "336h", &c.RefreshTTL},
		{"ORDER_CREATE_INTERVAL", "1m", &c.CreateInterval}, {"ORDER_UPDATE_INTERVAL", "1m", &c.UpdateInterval},
	} {
		d, err := time.ParseDuration(env(field.name, field.fallback))
		if err != nil || d < 0 {
			return c, fmt.Errorf("invalid duration in %s", field.name)
		}
		*field.target = d
	}
	if c.AccessTTL < 15*time.Minute || c.AccessTTL > 30*time.Minute {
		return c, fmt.Errorf("ACCESS_TOKEN_TTL must be between 15m and 30m")
	}
	if c.RefreshTTL < 7*24*time.Hour || c.RefreshTTL > 30*24*time.Hour {
		return c, fmt.Errorf("REFRESH_TOKEN_TTL must be between 168h and 720h")
	}
	if (c.AdminEmail == "") != (c.AdminPassword == "") {
		return c, fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be set together")
	}
	if c.AdminPassword != "" && (len(c.AdminPassword) < 8 || len(c.AdminPassword) > 72) {
		return c, fmt.Errorf("ADMIN_PASSWORD must contain 8 to 72 bytes")
	}
	return c, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
