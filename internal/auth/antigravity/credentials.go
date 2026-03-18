package antigravity

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func ResolveOAuthCredentials(cfg *config.Config) (string, string) {
	if cfg != nil {
		clientID := strings.TrimSpace(cfg.OAuthClients.Antigravity.ClientID)
		clientSecret := strings.TrimSpace(cfg.OAuthClients.Antigravity.ClientSecret)
		if clientID != "" || clientSecret != "" {
			return clientID, clientSecret
		}
	}
	return ClientID, ClientSecret
}
