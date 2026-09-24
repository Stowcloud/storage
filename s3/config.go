package s3

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/stowcloud/storage"
)

const maxPrefixBytes = 512

// Config is the secret-free configuration of an S3-compatible bucket.
type Config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key_id"`
	PathStyle bool   `json:"path_style"`
}

func ParseConfig(b []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("s3: parse config: %w", err)
	}
	if cfg.Bucket == "" {
		return Config{}, errors.New("s3: bucket is required")
	}
	if len(cfg.Prefix) > maxPrefixBytes {
		return Config{}, fmt.Errorf("s3: prefix exceeds %d bytes", maxPrefixBytes)
	}
	cfg.Prefix = strings.TrimSuffix(cfg.Prefix, "/")
	if err := validateConfigPath("bucket", cfg.Bucket); err != nil {
		return Config{}, err
	}
	if err := validateConfigPath("prefix", cfg.Prefix); err != nil {
		return Config{}, err
	}
	if !validRegion(cfg.Region) {
		return Config{}, fmt.Errorf("s3: region %q is not a lowercase [a-z0-9-]+ name", cfg.Region)
	}
	if err := validateEndpoint(cfg.Endpoint); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validRegion(s string) bool {
	if s == "" {
		return false
	}
	for i := range s {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func validateConfigPath(field, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("s3: %s is not valid UTF-8", field)
	}
	for i := range s {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("s3: %s contains a control character", field)
		}
	}
	if s == "" {
		return nil
	}
	if _, err := storage.ParsePath(s); err != nil {
		return fmt.Errorf("s3: %s: %w", field, err)
	}
	return nil
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("s3: endpoint: %w", err)
	}
	if !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("s3: endpoint must be an absolute http or https URL")
	}
	if u.Host == "" {
		return errors.New("s3: endpoint must name a host")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("s3: endpoint must not carry a path")
	}
	if u.RawQuery != "" {
		return errors.New("s3: endpoint must not carry a query")
	}
	if u.Fragment != "" {
		return errors.New("s3: endpoint must not carry a fragment")
	}
	if u.User != nil {
		return errors.New("s3: endpoint must not carry userinfo")
	}
	return nil
}

func (c Config) Marshal() ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("s3: marshal config: %w", err)
	}
	return b, nil
}

func (c Config) Describe() string {
	loc := "s3://" + c.Bucket
	if c.Prefix != "" {
		loc += "/" + c.Prefix
	}
	return loc + " at " + c.Endpoint
}
