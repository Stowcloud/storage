package s3

import (
	"encoding/json"
	"strings"
	"testing"
)

func configJSON(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	fields := map[string]any{"endpoint": "https://minio.example:9000", "region": "us-east-1", "bucket": "photos", "prefix": "team", "access_key_id": "AKIAEXAMPLE", "path_style": true}
	for k, v := range overrides {
		fields[k] = v
	}
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestParseConfigValidatesAndNormalizes(t *testing.T) {
	cfg, err := ParseConfig(configJSON(t, map[string]any{"prefix": "team/"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Prefix != "team" || cfg.Describe() != "s3://photos/team at https://minio.example:9000" {
		t.Fatalf("cfg=%+v", cfg)
	}
	b, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	round, err := ParseConfig(b)
	if err != nil || round != cfg {
		t.Fatalf("round trip=%+v,%v", round, err)
	}
}
func TestParseConfigRejectsTrustBoundaryInputs(t *testing.T) {
	cases := []map[string]any{{"bucket": ""}, {"endpoint": "minio.example:9000"}, {"endpoint": "ftp://minio.example"}, {"endpoint": "https://minio.example/v1"}, {"endpoint": "https://minio.example?x=1"}, {"bucket": "a/../b"}, {"bucket": "/photos"}, {"bucket": "photos\x01"}, {"prefix": "/team"}, {"prefix": "a\x00b"}, {"prefix": strings.Repeat("a", 513)}, {"region": "US-EAST-1"}, {"region": "us_east_1"}, {"region": ""}}
	for _, overrides := range cases {
		if _, err := ParseConfig(configJSON(t, overrides)); err == nil {
			t.Errorf("accepted %+v", overrides)
		}
	}
}
