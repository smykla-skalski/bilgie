package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoader_MergesUvxMinAgeOverDefaults(t *testing.T) {
	cfg, err := loadAgeConfig(t, "  uv:\n    uvx_min_age: 6h\n")
	require.NoError(t, err)
	assert.Equal(t, "6h", cfg.Providers["uv"].UvxMinAge)

	cfg, err = loadAgeConfig(t, "  uv:\n    enabled: true\n")
	require.NoError(t, err)
	assert.Empty(t, cfg.Providers["uv"].UvxMinAge, "the default applies in the provider")
}

func TestValidate_UvxMinAge(t *testing.T) {
	tests := []struct {
		name, provider, value, want string
	}{
		{"unset", "uv", "", ""},
		{"valid", "uv", "6h", ""},
		{"not a duration", "uv", "soon", `provider "uv": uvx_min_age`},
		{"other provider", "x", "2h", `uvx_min_age is only valid for the uv provider`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Providers: map[string]Provider{
				tt.provider: {Paths: []string{"/p"}, MaxSize: "1G", UvxMinAge: tt.value},
			}}
			err := cfg.Validate()
			if tt.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoader_RejectsUvxMinAgeUnderTheFloor(t *testing.T) {
	cfg, err := loadAgeConfig(t, "  uv:\n    uvx_min_age: 30s\n")
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "uvx_min_age 30s is under the 1m minimum")
}

func TestParseUvxMinAge(t *testing.T) {
	d, err := ParseUvxMinAge("")
	require.NoError(t, err)
	assert.Equal(t, time.Hour, d)

	d, err = ParseUvxMinAge("2d")
	require.NoError(t, err)
	assert.Equal(t, 48*time.Hour, d)
}
