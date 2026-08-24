package model

import "time"

// ExtractProviders contains the built-in providers that support fetching
// content from an already-known URL.
var ExtractProviders = []string{ProviderExa, ProviderJina, ProviderTavily, ProviderFirecrawl}

type ExtractFormat string

const (
	ExtractFormatMarkdown ExtractFormat = "markdown"
	ExtractFormatText     ExtractFormat = "text"
	ExtractFormatHTML     ExtractFormat = "html"
	ExtractFormatRawHTML  ExtractFormat = "raw_html"
)

type ExtractRequest struct {
	URLs               []string               `json:"urls"`
	Providers          []string               `json:"providers,omitempty"`
	ProvidersExplicit  bool                   `json:"-"`
	Mode               SearchMode             `json:"mode,omitempty"`
	Query              string                 `json:"query,omitempty"`
	Format             ExtractFormat          `json:"format,omitempty"`
	ExtractDepth       string                 `json:"extract_depth,omitempty"`
	ChunksPerSource    int                    `json:"chunks_per_source,omitempty"`
	ChunksPerSourceSet bool                   `json:"-"`
	IncludeImages      bool                   `json:"include_images,omitempty"`
	IncludeFavicon     bool                   `json:"include_favicon,omitempty"`
	IncludeRaw         bool                   `json:"include_raw,omitempty"`
	CompatFormat       CompatFormat           `json:"-"`
	Options            map[string]interface{} `json:"options,omitempty"`
}

type ExtractResponse struct {
	Results       []ExtractResult       `json:"results"`
	FailedResults []ExtractFailure      `json:"failed_results,omitempty"`
	Providers     []ProviderCallSummary `json:"providers"`
	Usage         []UsageMeasurement    `json:"usage,omitempty"`
	Meta          ExtractMetadata       `json:"meta"`
}

type ExtractResult struct {
	URL              string                 `json:"url"`
	Title            string                 `json:"title,omitempty"`
	Content          string                 `json:"content"`
	ContentTruncated bool                   `json:"content_truncated,omitempty"`
	Format           ExtractFormat          `json:"format,omitempty"`
	Provider         string                 `json:"provider"`
	Providers        []string               `json:"providers,omitempty"`
	Images           []string               `json:"images,omitempty"`
	ImagesTruncated  bool                   `json:"images_truncated,omitempty"`
	Favicon          string                 `json:"favicon,omitempty"`
	Author           string                 `json:"author,omitempty"`
	PublishedAt      *time.Time             `json:"published_at,omitempty"`
	Raw              map[string]interface{} `json:"raw,omitempty"`
	RawTruncated     bool                   `json:"raw_truncated,omitempty"`
}

type ExtractFailure struct {
	URL       string `json:"url"`
	Provider  string `json:"provider,omitempty"`
	ErrorType string `json:"error_type,omitempty"`
	Error     string `json:"error"`
}

type ExtractMetadata struct {
	RequestID         string       `json:"request_id"`
	Mode              SearchMode   `json:"mode"`
	CompatFormat      CompatFormat `json:"compat_format"`
	LatencyMS         int64        `json:"latency_ms"`
	TotalResults      int          `json:"total_results"`
	TotalFailed       int          `json:"total_failed"`
	ProvidersQueried  []string     `json:"providers_queried"`
	ResponseTruncated bool         `json:"response_truncated,omitempty"`
}

type ExtractProviderResponse struct {
	Results       []ExtractResult        `json:"results"`
	FailedResults []ExtractFailure       `json:"failed_results,omitempty"`
	Usage         []UsageMeasurement     `json:"usage,omitempty"`
	Raw           map[string]interface{} `json:"raw,omitempty"`
}
