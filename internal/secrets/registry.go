package secrets

import (
	"context"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ImageHost returns the registry host of an image reference, following Docker
// reference rules: the first path component is a host when it contains "." or
// ":" or equals "localhost"; otherwise the image lives on Docker Hub
// ("docker.io").
func ImageHost(image string) string {
	image = strings.TrimSpace(image)
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	first, _, found := strings.Cut(image, "/")
	if !found {
		return "docker.io"
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return canonicalRegistry(strings.ToLower(first))
	}
	return "docker.io"
}

func canonicalRegistry(h string) string {
	switch h {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com", "docker.io":
		return "docker.io"
	}
	return h
}

// TypeOf returns the type of the secret that Get resolves for name: the
// factory-scoped secret when one exists, else the team-scoped one. It returns
// "" when neither is listed. Store.List filters on the exact scope, so both
// scopes are listed.
func TypeOf(ctx context.Context, store core.SecretStore, teamID, factoryID, name string) core.SecretType {
	scopes := []string{factoryID}
	if factoryID != "" {
		scopes = append(scopes, "")
	}
	for _, scope := range scopes {
		list, err := store.List(ctx, teamID, scope)
		if err != nil {
			continue
		}
		for _, s := range list {
			if s.Name == name && s.FactoryID == scope {
				return s.Type
			}
		}
	}
	return ""
}
