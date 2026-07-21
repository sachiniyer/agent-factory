package config

import (
	"strings"
	"testing"
)

const trustedDockerDigest = "ghcr.io/example/af-session@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDockerEnvironmentImageTrustRequiresExactImmutableGrant(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		trusted []string
		want    bool
	}{
		{name: "exact digest", image: trustedDockerDigest, trusted: []string{trustedDockerDigest}, want: true},
		{name: "default deny", image: trustedDockerDigest},
		{name: "mutable tag cannot be trusted", image: "ghcr.io/example/af-session:latest", trusted: []string{"ghcr.io/example/af-session:latest"}},
		{name: "digest must match", image: trustedDockerDigest, trusted: []string{"ghcr.io/example/other@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		{name: "short digest", image: "ghcr.io/example/af-session@sha256:0123", trusted: []string{"ghcr.io/example/af-session@sha256:0123"}},
		{name: "non-sha256 digest", image: "ghcr.io/example/af-session@sha512:0123456789abcdef", trusted: []string{"ghcr.io/example/af-session@sha512:0123456789abcdef"}},
		{name: "uppercase digest", image: "ghcr.io/example/af-session@sha256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF", trusted: []string{"ghcr.io/example/af-session@sha256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"}},
		{name: "whitespace is not normalized into trust", image: " " + trustedDockerDigest, trusted: []string{trustedDockerDigest}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDockerEnvironmentImageTrusted(tt.image, tt.trusted); got != tt.want {
				t.Fatalf("IsDockerEnvironmentImageTrusted() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultConfigTrustsNoDockerImageWithHostEnvironment(t *testing.T) {
	if got := DefaultConfig().DockerEnvTrustedImages; len(got) != 0 {
		t.Fatalf("default Docker environment trust = %v, want none", got)
	}
}

func TestDockerEnvironmentImageTrustConfigLoadsOnlyImmutableDigests(t *testing.T) {
	cfg, err := parseConfigTOML([]byte(`docker_env_trusted_images = ["`+trustedDockerDigest+`"]
`), "config.toml")
	if err != nil {
		t.Fatalf("parse exact Docker trust grant: %v", err)
	}
	if len(cfg.DockerEnvTrustedImages) != 1 || cfg.DockerEnvTrustedImages[0] != trustedDockerDigest {
		t.Fatalf("loaded Docker trust grants = %v, want exact digest", cfg.DockerEnvTrustedImages)
	}

	_, err = parseConfigTOML([]byte(`docker_env_trusted_images = ["ghcr.io/example/af-session:latest"]
`), "config.toml")
	if err == nil {
		t.Fatal("mutable Docker image tag was accepted as an environment trust grant")
	}
	for _, want := range []string{"config.toml", "docker_env_trusted_images", "image@sha256", "entry 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mutable grant error %q does not contain %q", err, want)
		}
	}
}
