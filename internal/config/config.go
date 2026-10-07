// Package config loads runtime configuration from environment variables.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL       string
	RedisURL          string
	S3Endpoint        string
	S3Bucket          string
	S3AccessKey       string
	S3SecretKey       string
	S3UseSSL          bool
	ObjectLock        bool
	RetentionMode     string // GOVERNANCE or COMPLIANCE
	RetentionDays     int
	MasterKey         []byte
	AzureClientID     string
	AzureClientSecret string
	APIKeys           map[string]string // key -> role
	Listen            string
	ScheduleEvery     time.Duration
	WorkerConcurrency int
	MaxAttempts       int
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func atoi(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}

func Load() Config {
	sched, _ := time.ParseDuration(env("SCHEDULE_EVERY", "0"))
	c := Config{
		DatabaseURL:       env("DATABASE_URL", "postgres://m365:m365@localhost:5432/m365rescue?sslmode=disable"),
		RedisURL:          env("REDIS_URL", "redis://localhost:6379/0"),
		S3Endpoint:        env("S3_ENDPOINT", "localhost:9000"),
		S3Bucket:          env("S3_BUCKET", "m365rescue-backups"),
		S3AccessKey:       env("S3_ACCESS_KEY", "minioadmin"),
		S3SecretKey:       env("S3_SECRET_KEY", "minioadmin"),
		S3UseSSL:          env("S3_USE_SSL", "false") == "true",
		ObjectLock:        env("S3_OBJECT_LOCK", "true") == "true",
		RetentionMode:     strings.ToUpper(env("S3_RETENTION_MODE", "GOVERNANCE")),
		RetentionDays:     atoi(env("RETENTION_DAYS", "30"), 30),
		AzureClientID:     os.Getenv("AZURE_CLIENT_ID"),
		AzureClientSecret: os.Getenv("AZURE_CLIENT_SECRET"),
		Listen:            env("LISTEN_ADDR", ":8080"),
		ScheduleEvery:     sched,
		WorkerConcurrency: atoi(env("WORKER_CONCURRENCY", "4"), 4),
		MaxAttempts:       atoi(env("MAX_ATTEMPTS", "5"), 5),
		APIKeys:           map[string]string{},
	}
	mk := env("ENCRYPTION_MASTER_KEY", "dev-only-master-key-change-me")
	if b, err := hex.DecodeString(mk); err == nil && len(b) == 32 {
		c.MasterKey = b
	} else {
		s := sha256.Sum256([]byte(mk))
		c.MasterKey = s[:]
	}
	// API_KEYS="admin:key1,operator:key2,auditor:key3"
	for _, p := range strings.Split(env("API_KEYS", "admin:dev-admin-key,operator:dev-operator-key,auditor:dev-auditor-key"), ",") {
		if kv := strings.SplitN(strings.TrimSpace(p), ":", 2); len(kv) == 2 {
			c.APIKeys[kv[1]] = kv[0]
		}
	}
	return c
}
