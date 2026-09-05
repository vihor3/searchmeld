package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeHTTPOrigin(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "http://localhost:8080", want: "http://localhost:8080"},
		{input: "HTTPS://Admin.Example:443/", want: "https://admin.example"},
		{input: "http://Admin.Example:80/", want: "http://admin.example"},
		{input: "https://admin.example:8443", want: "https://admin.example:8443"},
		{input: "https://admin.example:00443", want: "https://admin.example"},
		{input: "https://admin.example./", want: "https://admin.example."},
		{input: "http://127.0.0.1:18080/", want: "http://127.0.0.1:18080"},
		{input: "http://[::1]:8080/", want: "http://[::1]:8080"},
		{input: "https://[2001:0DB8::1]:443", want: "https://[2001:db8::1]"},
		{input: "http://[::ffff:192.0.2.1]:80", want: "http://[::ffff:192.0.2.1]"},
		{input: "https://xn--bcher-kva.example", want: "https://xn--bcher-kva.example"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeHTTPOrigin(test.input)
			if err != nil || got != test.want {
				t.Fatalf("NormalizeHTTPOrigin(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
		})
	}
	for _, input := range []string{
		"", "null", "*", "https://*.example", "//admin.example", "admin.example", "ftp://admin.example",
		"https:", "https:admin.example", "https:///admin.example", "https://", "https://user:password@admin.example",
		"https://@admin.example", "https://admin.example/api/admin", "https://admin.example//", "https://admin.example/%2f",
		"https://admin.example?query=value", "https://admin.example?", "https://admin.example#fragment", "https://admin.example#",
		"https://admin.example,https://other.example", "https://admin.example https://other.example", " https://admin.example",
		"https://admin.example\t", "https://admin.example\n", "https://admin.example\\other", "https://admin.example:",
		"https://admin.example:0", "https://admin.example:65536", "https://admin.example:abc", "https://admin.example:-1",
		"https://[::1", "https://::1", "https://[127.0.0.1]", "https://[admin.example]", "http://[fe80::1%25eth0]",
		"https://.example", "https://a..example", "https://-admin.example", "https://admin-.example", "https://admin_example",
		"https://" + strings.Repeat("a", 64) + ".example",
	} {
		t.Run("invalid "+input, func(t *testing.T) {
			if got, err := NormalizeHTTPOrigin(input); err == nil || got != "" {
				t.Fatalf("NormalizeHTTPOrigin(%q) = %q, %v; want rejection", input, got, err)
			}
		})
	}
}

func TestLoadAdminPublicOrigin(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("ADMIN_USERNAME", "admin")
	t.Setenv("ADMIN_SESSION_TTL_HOURS", "24")
	withTempWorkingDir(t, nil)
	for _, test := range []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "", want: ""},
		{input: " HTTPS://Admin.Example:443/ ", want: "https://admin.example"},
		{input: "http://localhost:18080/", want: "http://localhost:18080"},
		{input: "https://user:private-value@admin.example", wantErr: true},
		{input: "https://admin.example/path", wantErr: true},
		{input: "*", wantErr: true},
	} {
		t.Run(test.input, func(t *testing.T) {
			t.Setenv("ADMIN_PUBLIC_ORIGIN", test.input)
			cfg, err := Load()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "ADMIN_PUBLIC_ORIGIN") || strings.Contains(err.Error(), "private-value") {
					t.Fatalf("Load() error = %v, want secret-safe origin validation error", err)
				}
				return
			}
			if err != nil || cfg.AdminPublicOrigin != test.want || cfg.AdminSessionTTL != 24*time.Hour {
				t.Fatalf("Load(): origin=%q ttl=%v error=%v", cfg.AdminPublicOrigin, cfg.AdminSessionTTL, err)
			}
		})
	}
}

func TestValidateAdminPublicOrigin(t *testing.T) {
	cfg := Config{AdminUsername: "admin", AdminSessionTTL: time.Hour}
	for _, origin := range []string{"", "https://admin.example", "http://localhost:18080/"} {
		cfg.AdminPublicOrigin = origin
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid origin %q rejected: %v", origin, err)
		}
	}
	cfg.AdminPublicOrigin = "https://admin.example?"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ADMIN_PUBLIC_ORIGIN") {
		t.Fatalf("invalid configured origin error = %v", err)
	}
}

func TestLoadEnvFilesLetsDevelopmentOverrideBaseEnv(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	withTempWorkingDir(t, map[string]string{
		".env":             "DATABASE_URL=postgres://one_search:prod@localhost:15432/one_search?sslmode=disable\nHTTP_ADDR=:8080\n",
		".env.development": "DATABASE_URL=postgres://one_search:dev@localhost:15432/one_search?sslmode=disable\nHTTP_ADDR=:18080\n",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !strings.Contains(cfg.DatabaseURL, ":dev@") {
		t.Fatalf("DatabaseURL = %q, want development override", cfg.DatabaseURL)
	}
	if cfg.HTTPAddr != ":18080" {
		t.Fatalf("HTTPAddr = %q, want :18080", cfg.HTTPAddr)
	}
}

func TestLoadEnvFilesKeepsExplicitEnvironmentValues(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://one_search:explicit@localhost:15432/one_search?sslmode=disable")
	withTempWorkingDir(t, map[string]string{
		".env":             "DATABASE_URL=postgres://one_search:prod@localhost:15432/one_search?sslmode=disable\n",
		".env.development": "DATABASE_URL=postgres://one_search:dev@localhost:15432/one_search?sslmode=disable\n",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !strings.Contains(cfg.DatabaseURL, ":explicit@") {
		t.Fatalf("DatabaseURL = %q, want explicit env value", cfg.DatabaseURL)
	}
}

func withTempWorkingDir(t *testing.T, files map[string]string) {
	t.Helper()

	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWd); err != nil {
			t.Fatalf("os.Chdir(%q) error = %v", originalWd, err)
		}
	})

	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", name, err)
		}
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("os.Chdir(%q) error = %v", dir, err)
	}
}
