package db

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestConnectRejectsIncompatibleQueryOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options string
		want    string
	}{
		{name: "simple enabled", options: "prefer_simple_protocol=true", want: "default_query_exec_mode=simple_protocol"},
		{name: "simple disabled", options: "prefer_simple_protocol=false", want: "default extended protocol"},
		{name: "prepare cache", options: "statement_cache_mode=prepare", want: "default_query_exec_mode=cache_statement"},
		{name: "describe cache", options: "statement_cache_mode=describe", want: "default_query_exec_mode=cache_describe"},
		{name: "zero statement cache", options: "statement_cache_capacity=0", want: "default_query_exec_mode=describe_exec"},
		{name: "negative statement cache", options: "statement_cache_capacity=-1", want: "statement_cache_capacity must be positive"},
		{name: "zero description cache", options: "default_query_exec_mode=cache_describe&description_cache_capacity=0", want: "description_cache_capacity must be positive"},
		{name: "negative description cache", options: "default_query_exec_mode=cache_describe&description_cache_capacity=-1", want: "description_cache_capacity must be positive"},
		{name: "conflicting legacy mode", options: "prefer_simple_protocol=true&default_query_exec_mode=exec", want: "prefer_simple_protocol is unsupported"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params, err := url.ParseQuery(tt.options)
			if err != nil {
				t.Fatalf("parse fixture options: %v", err)
			}
			params.Set("sslmode", "disable")
			databaseURL := url.URL{
				Scheme:   "postgres",
				User:     url.UserPassword("fixture", "secret-sentinel"),
				Host:     "127.0.0.1:5432",
				Path:     "/fixture",
				RawQuery: params.Encode(),
			}
			for _, connString := range []string{
				databaseURL.String(),
				"host=127.0.0.1 port=5432 user=fixture password=secret-sentinel dbname=fixture sslmode=disable " + strings.ReplaceAll(tt.options, "&", " "),
			} {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				pool, err := Connect(ctx, connString)
				if pool != nil {
					pool.Close()
					t.Fatal("incompatible options returned a pool")
				}
				if err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Connect error = %v, want migration guidance containing %q", err, tt.want)
				}
				if strings.Contains(err.Error(), "secret-sentinel") {
					t.Fatal("database option error exposed the connection password")
				}
			}
		})
	}
}

func TestConnectAcceptsExplicitQueryModes(t *testing.T) {
	for _, mode := range []string{"cache_statement", "cache_describe", "describe_exec", "exec", "simple_protocol"} {
		zeroUnusedCaches := "statement_cache_capacity=0&description_cache_capacity=0"
		switch mode {
		case "cache_statement":
			zeroUnusedCaches = "description_cache_capacity=0"
		case "cache_describe":
			zeroUnusedCaches = "statement_cache_capacity=0"
		}
		for _, options := range []string{"", zeroUnusedCaches} {
			t.Run(mode+"/"+options, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				pool, err := Connect(ctx, "postgres://fixture@127.0.0.1:5432/fixture?sslmode=disable&default_query_exec_mode="+mode+"&"+options)
				if pool != nil {
					pool.Close()
					t.Fatal("Connect returned a pool without a successful startup ping")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("valid v5 mode/cache combination rejected before startup ping: %v", err)
				}
			})
		}
	}
}
